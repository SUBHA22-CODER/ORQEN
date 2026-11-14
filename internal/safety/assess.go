package safety

import (
	"fmt"
	"sort"
	"strings"
)

// Safety Score Assessment: deterministic, explainable scoring over collected
// evidence. Every number here is computed from evidence and the proposed
// action only — an LLM cannot assign or inflate any score, because none of
// these functions accept a model-provided value.
//
// Three independent measures are reported:
//   - Diagnostic confidence (0-100): how strongly the evidence supports the
//     diagnosis (quality x reproducibility x agreement, minus conflicts).
//   - Repair risk (0-100): how dangerous the proposed action is (action
//     base + reversibility + blast radius + data-loss markers).
//   - Evidence sufficiency (0-100): whether the evidence collected is enough
//     to justify acting at all — reported separately because sufficiency and
//     confidence answer different questions.

// RiskScore computes the numeric repair risk (0-100) and the human-readable
// list of factors that produced it.
func RiskScore(in RiskInput) (int, []string) {
	var factors []string
	base := 40
	switch in.Action {
	case ActionRestart:
		base = 20
	case ActionManifestPatch:
		base = 30
	case ActionMultiManifest:
		base = 50
	case ActionInfraChange:
		base = 50
	case ActionDataDestructive:
		base = 100
	default:
		base = 40
	}
	factors = append(factors, fmt.Sprintf("action %s: base risk %d", in.Action, base))

	score := base
	if len(in.DestructiveMarkers) > 0 || in.Action == ActionDataDestructive {
		score = 100
		factors = append(factors, fmt.Sprintf("data-loss markers present (%s): risk forced to 100", strings.Join(in.DestructiveMarkers, ", ")))
	}
	if !in.Reversible {
		score += 30
		factors = append(factors, "not reversible: +30")
	}
	if len(in.ServicesTouched) > 1 {
		score += 15
		factors = append(factors, fmt.Sprintf("affects %d services: +15", len(in.ServicesTouched)))
	}
	if len(in.FilesTouched) > 2 {
		score += 15
		factors = append(factors, fmt.Sprintf("touches %d files: +15", len(in.FilesTouched)))
	}
	if in.Reversible && len(in.DestructiveMarkers) == 0 && in.Action != ActionDataDestructive {
		factors = append(factors, "reversible (pristine snapshot available): no penalty")
	}
	if score > 100 {
		score = 100
	}
	return score, factors
}

// levelFromScore maps a numeric risk (0-100) to its RiskLevel band.
func levelFromScore(score int) RiskLevel {
	switch {
	case score < 40:
		return RiskLow
	case score < 60:
		return RiskMedium
	case score < 85:
		return RiskHigh
	default:
		return RiskCritical
	}
}

// EvidenceSufficiency scores whether the collected evidence is enough to
// justify acting (0-100), separately from confidence, with explanations.
func EvidenceSufficiency(ev Set, repetitions int) (int, []string) {
	var factors []string
	if len(ev.Items) == 0 {
		return 0, []string{"no evidence collected"}
	}
	quality := 0.0
	var strongest Evidence
	for _, e := range ev.Items {
		if e.Source == SourceModel {
			continue
		}
		if w := e.Weight(); w > quality {
			quality = w
			strongest = e
		}
	}
	if quality == 0 {
		return 0, []string{"only model-derived text present; no deterministic observations"}
	}

	// Problem-kind coverage: distinct evidence kinds carrying a problem
	// signature, normalized against the three strongest diagnostic kinds.
	problemKinds := map[string]bool{}
	var problemKindNames []string
	for _, e := range ev.Problems() {
		if !problemKinds[string(e.Kind)] {
			problemKinds[string(e.Kind)] = true
			problemKindNames = append(problemKindNames, string(e.Kind))
		}
	}
	sort.Strings(problemKindNames)
	coverage := float64(len(problemKinds)) / 3.0
	if coverage > 1 {
		coverage = 1
	}

	reps := repetitions
	if reps < 1 {
		reps = 1
	}
	repro := float64(reps) / 3.0
	if repro > 1 {
		repro = 1
	}

	factors = append(factors, fmt.Sprintf("strongest signal: %s (weight %.2f)", strongest.Kind, quality))
	factors = append(factors, fmt.Sprintf("problem kinds observed: %d (%s)", len(problemKinds), strings.Join(problemKindNames, ", ")))
	factors = append(factors, fmt.Sprintf("repetitions: %d of 3 observations", reps))
	if len(ev.Problems()) == 0 {
		factors = append(factors, "no problem signature observed: evidence cannot support a repair diagnosis")
	}

	score := int(100 * clamp01(0.45*quality+0.25*repro+0.30*coverage))
	return score, factors
}

// SufficiencyTier classifies an evidence-sufficiency score.
func SufficiencyTier(score int) string {
	switch {
	case score >= 70:
		return "SUFFICIENT"
	case score >= 45:
		return "PARTIAL"
	default:
		return "INSUFFICIENT"
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ScoreReport is the complete, explainable assessment of one situation.
type ScoreReport struct {
	Confidence         int      `json:"confidence"` // 0-100
	ConfidenceTier     string   `json:"confidence_tier"`
	ConfidenceFactors  []string `json:"confidence_factors"`
	Risk               int      `json:"risk"` // 0-100
	RiskLabel          string   `json:"risk_label"`
	RiskFactors        []string `json:"risk_factors"`
	Sufficiency        int      `json:"sufficiency"` // 0-100
	SufficiencyTier    string   `json:"sufficiency_tier"`
	SufficiencyFactors []string `json:"sufficiency_factors"`
	Decision           Decision `json:"decision"`
	DecisionReason     string   `json:"decision_reason"`
	EvidenceLines      []string `json:"evidence_lines"`
}

// AssessEvidence produces the full report for one evidence set and one
// prospective action. repetitions is how many independent observations saw
// the same problem (1 for a point-in-time assessment).
func AssessEvidence(ev Set, repetitions int, riskIn RiskInput) ScoreReport {
	// Confidence: computed exclusively from deterministic evidence.
	agree := agreeKinds(ev)
	total := len(ev.Kinds())
	conf := Score(ConfidenceInput{
		Evidence:     ev,
		Repetitions:  repetitions,
		SignalsAgree: agree,
		SignalsTotal: total,
	})

	confFactors := []string{}
	for _, e := range ev.Problems() {
		confFactors = append(confFactors, fmt.Sprintf("signal %s[%s] %s: weight %.2f", e.Kind, e.Source, e.Signature, e.Weight()))
	}
	if len(confFactors) == 0 {
		confFactors = append(confFactors, "no problem signals in the evidence: confidence rests on nothing")
	}
	confFactors = append(confFactors,
		fmt.Sprintf("agreement: %d of %d independent signal kinds point at the problem", agree, total),
		fmt.Sprintf("reproducibility: %d of 3 observations", max1(repetitions)),
	)
	if len(ev.Problems()) == 0 {
		confFactors = append(confFactors, "model-only support is capped: an LLM assertion can never raise confidence alone")
	}

	// Risk: numeric score with factor attribution.
	riskScore, riskFactors := RiskScore(riskIn)
	riskLevel := levelFromScore(riskScore)

	// Sufficiency: separate measure, own tier.
	suffScore, suffFactors := EvidenceSufficiency(ev, repetitions)

	// Final policy decision through the same engine the healer uses.
	decision, reason := Decide(DecisionInput{
		Transience:    PersistentFault,
		EvidenceKinds: ev.Kinds(),
		Confidence:    conf,
		Risk:          riskLevel,
		Destructive:   len(riskIn.DestructiveMarkers) > 0,
	})

	var evidenceLines []string
	for _, e := range ev.Items {
		evidenceLines = append(evidenceLines, e.String())
	}

	return ScoreReport{
		Confidence:         int(conf * 100),
		ConfidenceTier:     ConfidenceTier(conf),
		ConfidenceFactors:  confFactors,
		Risk:               riskScore,
		RiskLabel:          riskLevel.RiskLabel(),
		RiskFactors:        riskFactors,
		Sufficiency:        suffScore,
		SufficiencyTier:    SufficiencyTier(suffScore),
		SufficiencyFactors: suffFactors,
		Decision:           decision,
		DecisionReason:     reason,
		EvidenceLines:      evidenceLines,
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// --- deterministic safety-test scenarios ------------------------------------

// SafetyScenario is a fixed fixture: the same inputs always produce the same
// report and the same decision.
type SafetyScenario struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Evidence    Set       `json:"-"`
	Repetitions int       `json:"repetitions"`
	RiskInput   RiskInput `json:"-"`
	Expected    Decision  `json:"expected"`
	Expectation string    `json:"expectation"`
}

// ScenarioResult is one scenario's outcome against its expectation.
type ScenarioResult struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Report      ScoreReport `json:"report"`
	Expected    Decision    `json:"expected"`
	Expectation string      `json:"expectation"`
	Pass        bool        `json:"pass"`
	Status      string      `json:"status"` // PASS | FAIL | SKIPPED
	Detail      string      `json:"detail,omitempty"`
}

// SafetySummary aggregates a safety-test run.
type SafetySummary struct {
	Results []ScenarioResult `json:"results"`
	Passed  int              `json:"passed"`
	Failed  int              `json:"failed"`
	Skipped int              `json:"skipped"`
}

// scenarioEvidence builds the fixed evidence fixtures. Deterministic: no
// clocks, no randomness, no I/O.
func scenarioEvidence(kinds ...string) Set {
	ev := Set{}
	for _, k := range kinds {
		switch k {
		case "container":
			ev.Add(Evidence{
				ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api",
				Detail: "state=restarting exit=1", Signature: "restart_loop:api:exit=1",
			})
		case "http":
			ev.Add(Evidence{
				ID: "http:api", Kind: KindHTTP, Source: SourceProbe, Service: "api",
				Detail: "application_error code=500", Signature: "http_application_error:api",
			})
		case "log":
			ev.Add(Evidence{
				ID: "log:api", Kind: KindLog, Source: SourceLog, Service: "api",
				Detail: "error: connection refused", Signature: "log_connection_refused:api",
			})
		case "dependency":
			ev.Add(Evidence{
				ID: "dependency:postgres", Kind: KindDependency, Source: SourceDocker, Service: "postgres",
				Healthy: true, Detail: "state=running health=healthy",
			})
		}
	}
	return ev
}

// SafetyScenarios returns the three deterministic assessment scenarios.
func SafetyScenarios() map[string]SafetyScenario {
	safeRisk := RiskInput{
		Action:          ActionManifestPatch,
		FilesTouched:    []string{"Dockerfile.api"},
		ServicesTouched: []string{"api"},
		Reversible:      true,
	}
	highRisk := RiskInput{
		Action:          ActionMultiManifest,
		FilesTouched:    []string{"docker-compose.yml", "Dockerfile.api", "Dockerfile.web"},
		ServicesTouched: []string{"api", "web"},
		Reversible:      true,
	}
	emptyRisk := RiskInput{
		Action:          ActionManifestPatch,
		FilesTouched:    []string{"Dockerfile.api"},
		ServicesTouched: []string{"api"},
		Reversible:      true,
	}
	return map[string]SafetyScenario{
		"safe": {
			Name:        "safe",
			Description: "Strong corroborated evidence (container + HTTP, 3 observations), reversible single-file fix",
			Evidence:    scenarioEvidence("container", "http"),
			Repetitions: 3,
			RiskInput:   safeRisk,
			Expected:    AutoApply,
			Expectation: "sufficient evidence + low risk + high confidence = eligible for automatic validated repair",
		},
		"high-risk": {
			Name:        "high-risk",
			Description: "Same strong evidence, but the proposed action touches three files across two services",
			Evidence:    scenarioEvidence("container", "http"),
			Repetitions: 3,
			RiskInput:   highRisk,
			Expected:    RequireApproval,
			Expectation: "high risk = human approval required, regardless of confidence",
		},
		"insufficient-evidence": {
			Name:        "insufficient-evidence",
			Description: "No deterministic evidence collected at all",
			Evidence:    Set{},
			Repetitions: 0,
			RiskInput:   emptyRisk,
			Expected:    NoAction,
			Expectation: "insufficient evidence = investigation or blocked repair, never automatic modification",
		},
	}
}

// RunSafetyScenario executes one scenario and compares against expectation.
func RunSafetyScenario(s SafetyScenario) ScenarioResult {
	report := AssessEvidence(s.Evidence, s.Repetitions, s.RiskInput)
	res := ScenarioResult{
		Name:        s.Name,
		Description: s.Description,
		Report:      report,
		Expected:    s.Expected,
		Expectation: s.Expectation,
		Status:      "FAIL",
	}
	res.Pass = report.Decision == s.Expected
	if res.Pass {
		res.Status = "PASS"
	}
	// Explicit eligibility cross-check for the safe scenario: the decision
	// alone is not enough; the eligibility conditions must hold too.
	if s.Name == "safe" && res.Pass {
		if !(report.Confidence >= 70 && report.Risk < 40 && report.Sufficiency >= 70) {
			res.Pass = false
			res.Status = "FAIL"
			res.Detail = fmt.Sprintf("eligibility conditions not met: confidence=%d risk=%d sufficiency=%d", report.Confidence, report.Risk, report.Sufficiency)
		}
	}
	if s.Name == "insufficient-evidence" && report.Decision == AutoApply {
		res.Pass = false
		res.Status = "FAIL"
		res.Detail = "insufficient evidence must never produce automatic modification"
	}
	return res
}

// RunSafetyScenarios runs the named scenarios (or all when empty) and
// aggregates PASS/FAIL/SKIPPED counts.
func RunSafetyScenarios(names ...string) SafetySummary {
	all := SafetyScenarios()
	if len(names) == 0 {
		for name := range all {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	summary := SafetySummary{}
	for _, name := range names {
		s, ok := all[name]
		if !ok {
			summary.Results = append(summary.Results, ScenarioResult{
				Name: name, Status: "SKIPPED",
				Detail: fmt.Sprintf("unknown scenario %q (valid: safe, high-risk, insufficient-evidence)", name),
			})
			summary.Skipped++
			continue
		}
		r := RunSafetyScenario(s)
		summary.Results = append(summary.Results, r)
		switch r.Status {
		case "PASS":
			summary.Passed++
		case "FAIL":
			summary.Failed++
		default:
			summary.Skipped++
		}
	}
	return summary
}
