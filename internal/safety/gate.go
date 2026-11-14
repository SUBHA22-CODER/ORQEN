package safety

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Gate is the Repair Safety Engine orchestrator. It is attached to the healer
// as an optional field; a nil gate preserves the legacy behavior exactly.
//
// Flow:
//  1. PreFlight — collect evidence, sample for transience, consult the
//     circuit breaker, and rule whether healing may proceed at all.
//  2. ReviewPlan — for every proposed (especially AI-generated) patch:
//     cross-check the diagnosis against evidence, sandbox-validate the files,
//     score confidence and risk independently, and require approval when the
//     action is risky or destructive.
//  3. RecordOutcome — feed the result back into breaker and audit.
type Gate struct {
	Collector *Collector
	Sampler   *Sampler
	Validator Validator
	Breaker   *Breaker
	Audit     *Audit
	Sandbox   *Sandbox
	// Approver is consulted when a repair requires user approval. Returning
	// false (or a nil Approver) rejects the repair: the safe default.
	Approver func(ctx context.Context, req ApprovalRequest) bool
	// Coordinator, when set, replaces Approver with the structured judge
	// approval backend (approval IDs, action-hash binding, persistence, SSE
	// events). It takes precedence over Approver.
	Coordinator *ApprovalCoordinator
	Logf        func(format string, args ...any)
	// MaxInvestigateRounds bounds autonomous deeper-diagnostic rounds.
	MaxInvestigateRounds int
}

// ApprovalRequest is what the user is asked to approve. Every field is
// computed by the gate from deterministic state: the LLM never supplies its
// own confidence, evidence, or authorization.
type ApprovalRequest struct {
	Signature   string   `json:"signature"`
	Summary     string   `json:"summary"`
	Risk        string   `json:"risk"`
	Explanation string   `json:"explanation"`
	Files       []string `json:"files"`
	Diagnosis   string   `json:"diagnosis,omitempty"`
	// Independent classification and binding (gate-computed).
	Confidence   float64  `json:"confidence"`
	Uncertain    bool     `json:"uncertain,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	EvidenceHash string   `json:"evidence_hash,omitempty"`
	// ActionHash binds the approval to the exact proposed bytes.
	ActionHash string `json:"action_hash,omitempty"`
}

// PlanDecision is the ReviewPlan outcome.
type PlanDecision struct {
	Decision    Decision       `json:"decision"`
	Conflicts   []Conflict     `json:"conflicts,omitempty"`
	Risk        RiskLevel      `json:"risk"`
	Confidence  float64        `json:"confidence"`
	Sandbox     *SandboxResult `json:"sandbox,omitempty"`
	Explanation string         `json:"explanation"`
}

func (g *Gate) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

func (g *Gate) sampler() *Sampler {
	if g.Sampler != nil {
		return g.Sampler
	}
	return &Sampler{Collector: g.Collector, Samples: 3, Interval: time.Second}
}

// PreFlight observes the stack, classifies the fault, and rules on whether
// healing may proceed. It never modifies anything. The returned verdict
// carries a plain-language explanation for non-technical users.
func (g *Gate) PreFlight(ctx context.Context, signature string) (Verdict, error) {
	v := Verdict{}
	if g.Collector == nil {
		return v, fmt.Errorf("safety.Gate.PreFlight: nil collector")
	}

	// Circuit breaker first: repeated failures pause autonomous healing
	// regardless of how confident the current diagnosis looks.
	if g.Breaker != nil && signature != "" {
		if ok, why := g.Breaker.Allow(signature); !ok {
			v.Decision = NoAction
			v.Reason = why
			v.BreakerOpen = true
			v.Signature = signature
			g.audit(Record{
				Signature: signature, Phase: "preflight", Decision: NoAction.String(),
				Reason: why, Outcome: "nothing_to_do",
			})
			return v, nil
		}
	}

	first, err := g.Collector.Collect(ctx)
	if err != nil {
		return v, err
	}
	v.Evidence = first
	v.Signature = dominantSignature(first)

	// Autonomous investigation: when the first observation shows trouble,
	// re-observe to separate transient from persistent faults instead of
	// asking the user to diagnose anything.
	final := first
	transience := TransientUnknown
	if len(first.Signatures()) > 0 {
		final, transience, err = g.sampler().Sample(ctx, first)
		if err != nil {
			return v, err
		}
		v.Evidence = final
		v.Transience = transience
	}

	// Evidence sufficiency: a healthy stack means nothing to heal.
	if len(final.Problems()) == 0 {
		v.Decision = NoAction
		v.Reason = "no persistent problem observed: all deterministic signals healthy"
		if transience == TransientFault {
			v.Reason = "observed fault is transient (recovered on re-observation); no modification performed"
		}
		v.Transience = transience
		g.audit(Record{
			Signature: signature, Phase: "preflight", Decision: NoAction.String(),
			Reason: v.Reason, EvidenceHash: HashEvidence(final),
			EvidenceSummary: summarize(final), Outcome: "nothing_to_do",
		})
		return v, nil
	}

	// Confidence from evidence quality, reproducibility, and agreement.
	reps := 1
	if transience == PersistentFault {
		reps = g.sampler().Samples
		if reps < 2 {
			reps = 2
		}
	}
	v.Confidence = Score(ConfidenceInput{
		Evidence:     final,
		Repetitions:  reps,
		SignalsAgree: agreeKinds(final),
		SignalsTotal: len(final.Kinds()),
	})
	v.Decision, v.Reason = Decide(DecisionInput{
		Transience:    transience,
		EvidenceKinds: final.Kinds(),
		Confidence:    v.Confidence,
		Risk:          RiskLow, // preflight rules on investigation, not on a patch
	})
	if v.Decision == AutoApply {
		// Preflight never applies anything; AutoApply here means "cleared to
		// proceed into the repair loop".
		v.Decision = Investigate
		v.Reason += "; cleared to proceed with the repair loop"
	}
	g.audit(Record{
		Signature: signature, Phase: "preflight", Decision: v.Decision.String(),
		Reason: v.Reason, EvidenceHash: HashEvidence(final),
		EvidenceSummary: summarize(final), Confidence: v.Confidence,
		Outcome: outcomeFor(v.Decision),
	})
	return v, nil
}

// ReviewPlan evaluates a proposed patch (typically AI-generated): diagnosis
// cross-check, sandbox validation, independent confidence/risk scoring, and
// the approval requirement. It returns the ruling plus an explanation suitable
// for feedback to the model and the audit trail. It never applies anything.
func (g *Gate) ReviewPlan(ctx context.Context, v Verdict, d Diagnosis, p Proposal) PlanDecision {
	pd := PlanDecision{Decision: NoAction}

	// 1. Cross-check diagnosis claims against deterministic evidence.
	conflicts := g.Validator.CrossCheck(d, v.Evidence)
	pd.Conflicts = conflicts
	if len(conflicts) > 0 {
		pd.Decision = Investigate
		pd.Explanation = "diagnosis rejected: contradicted by deterministic evidence:\n" + ConflictsSummary(conflicts)
		g.auditPlan(v, d, p, pd, "rejected")
		return pd
	}

	// 2. Independent confidence: deterministic support required.
	modelOnly := len(v.Evidence.Problems()) == 0
	reps := 1
	if v.Transience == PersistentFault {
		reps = g.sampler().Samples
		if reps < 2 {
			reps = 2
		}
	}
	pd.Confidence = Score(ConfidenceInput{
		Evidence:     v.Evidence,
		Repetitions:  reps,
		SignalsAgree: agreeKinds(v.Evidence),
		SignalsTotal: len(v.Evidence.Kinds()),
		Conflicts:    len(conflicts),
		ModelOnly:    modelOnly,
	})

	// 3. Risk from the action itself, never from model claims.
	files := make([]string, 0, len(p.Files))
	for f := range p.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	joined := ""
	for _, f := range files {
		joined += p.Files[f] + "\n"
	}
	markers := DetectDestructiveMarkers(joined)
	riskIn := RiskInput{
		Action:             riskAction(files),
		FilesTouched:       files,
		ServicesTouched:    servicesFromFiles(files, p.Service),
		Reversible:         true, // healer snapshots manifests before any write
		DestructiveMarkers: markers,
	}
	pd.Risk = Assess(riskIn)

	// 4. Sandbox validation of the proposed files in isolation.
	sb := g.Sandbox.Validate(ctx, p)
	pd.Sandbox = &sb
	if !sb.Valid {
		pd.Decision = NoAction
		pd.Explanation = "patch rejected by sandbox validation: " + sb.Detail
		g.auditPlan(v, d, p, pd, "rejected")
		return pd
	}

	// 5. Policy ruling.
	pd.Decision, pd.Explanation = Decide(DecisionInput{
		Transience:    v.Transience,
		EvidenceKinds: v.Evidence.Kinds(),
		Confidence:    pd.Confidence,
		Risk:          pd.Risk,
		Conflicts:     len(conflicts),
		Destructive:   len(markers) > 0,
	})

	// 6. Approval for anything the independent policy flags. The gate computes
	// every binding field itself; the model never self-authorizes.
	if pd.Decision == RequireApproval {
		req := ApprovalRequest{
			Signature:   v.Signature,
			Summary:     p.Summary,
			Risk:        pd.Risk.RiskLabel(),
			Explanation: pd.Explanation,
			Files:       files,
			Diagnosis:   d.Raw,
			Confidence:  pd.Confidence,
			Uncertain: UncertainButActionable(DecisionInput{
				Confidence:    pd.Confidence,
				Risk:          pd.Risk,
				EvidenceKinds: v.Evidence.Kinds(),
				Conflicts:     len(conflicts),
				Destructive:   len(markers) > 0,
			}),
			Evidence:     summarize(v.Evidence),
			EvidenceHash: HashEvidence(v.Evidence),
			ActionHash:   HashAction(string(riskAction(files)), p.Files),
		}
		ask := g.Approver
		if g.Coordinator != nil {
			ask = g.Coordinator.Approver
		}
		if ask == nil || !ask(ctx, req) {
			pd.Decision = NoAction
			pd.Explanation = "repair requires user approval and was not approved; nothing was modified"
			g.auditPlan(v, d, p, pd, "rejected")
			return pd
		}
		pd.Explanation += " (approved by user)"
		g.auditPlan(v, d, p, pd, "approved")
		return pd
	}

	g.auditPlan(v, d, p, pd, outcomeFor(pd.Decision))
	return pd
}

// RecordOutcome feeds a healing attempt result into the circuit breaker and
// the audit trail, and closes any approval lifecycle opened by this run.
func (g *Gate) RecordOutcome(signature string, success bool, detail string) {
	if g.Coordinator != nil {
		verification := detail
		if success {
			verification = "stack rebuilt and health verified"
		}
		g.Coordinator.CompleteAll(success, verification, "generated manifests restored from snapshot on failure")
	}
	if g.Breaker != nil {
		g.Breaker.RecordOutcome(signature, success, detail)
	}
	outcome := "failed"
	if success {
		outcome = "applied"
	}
	g.audit(Record{
		Signature: signature, Phase: "outcome",
		Decision: decisionWord(success), Reason: detail, Outcome: outcome,
	})
}

func decisionWord(success bool) string {
	if success {
		return AutoApply.String()
	}
	return NoAction.String()
}

func (g *Gate) audit(rec Record) {
	if g.Audit == nil {
		return
	}
	g.Audit.Append(rec)
}

func (g *Gate) auditPlan(v Verdict, d Diagnosis, p Proposal, pd PlanDecision, outcome string) {
	hashes := map[string]string{}
	for f, c := range p.Files {
		hashes[f] = HashString(c)
	}
	var conflicts []string
	for _, c := range pd.Conflicts {
		conflicts = append(conflicts, c.Explanation)
	}
	g.audit(Record{
		Signature: v.Signature, Phase: "review", Decision: pd.Decision.String(),
		Reason:          pd.Explanation,
		EvidenceHash:    HashEvidence(v.Evidence),
		EvidenceSummary: summarize(v.Evidence),
		Diagnosis:       bound(d.Raw, 2000),
		Conflicts:       conflicts,
		Confidence:      pd.Confidence,
		Risk:            pd.Risk.String(),
		Validation:      pd.Sandbox,
		PatchHashes:     hashes,
		Outcome:         outcome,
	})
}

// riskAction classifies the patch shape into an ActionKind.
func riskAction(files []string) ActionKind {
	if len(files) == 0 {
		return ActionRestart
	}
	if len(files) == 1 {
		return ActionManifestPatch
	}
	return ActionMultiManifest
}

func servicesFromFiles(files []string, primary string) []string {
	set := map[string]bool{}
	if primary != "" {
		set[primary] = true
	}
	for _, f := range files {
		if s := strings.TrimPrefix(f, "Dockerfile."); s != f && s != "" {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// dominantSignature picks the most common signature (ties: first seen).
func dominantSignature(s Set) string {
	count := map[string]int{}
	order := []string{}
	for _, e := range s.Problems() {
		if count[e.Signature] == 0 {
			order = append(order, e.Signature)
		}
		count[e.Signature]++
	}
	best, bestN := "", 0
	for _, sig := range order {
		if count[sig] > bestN {
			best, bestN = sig, count[sig]
		}
	}
	return best
}

// agreeKinds counts how many evidence kinds contain at least one problem
// signature: independent signals agreeing that something is wrong.
func agreeKinds(s Set) int {
	kinds := map[string]bool{}
	for _, e := range s.Problems() {
		kinds[string(e.Kind)] = true
	}
	return len(kinds)
}

// summarize renders bounded human-readable evidence lines for the audit.
func summarize(s Set) []string {
	var out []string
	for _, e := range s.Items {
		if e.Signature != "" || !e.Healthy {
			out = append(out, e.String())
		}
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func outcomeFor(d Decision) string {
	switch d {
	case AutoApply:
		return "applied"
	case NoAction:
		return "nothing_to_do"
	default:
		return "investigating"
	}
}
