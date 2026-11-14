package safety

import (
	"fmt"
	"strings"
)

// Decision is the safety engine's final ruling for a proposed repair.
type Decision int

const (
	// AutoApply: safe, reversible, evidence-supported — execute now.
	AutoApply Decision = iota
	// Investigate: ambiguous — gather more evidence and re-evaluate
	// autonomously; the user is never asked to debug.
	Investigate
	// RequireApproval: risky or destructive — a human must approve.
	RequireApproval
	// NoAction: insufficient evidence or unsafe conditions — modify nothing.
	NoAction
)

// String renders the decision for logs and audit records.
func (d Decision) String() string {
	switch d {
	case AutoApply:
		return "auto_apply"
	case Investigate:
		return "investigate"
	case RequireApproval:
		return "require_approval"
	default:
		return "no_action"
	}
}

// DecisionInput carries everything the policy weighs.
type DecisionInput struct {
	// Transience is the sampler's classification of the observed problem.
	Transience Transience
	// EvidenceKinds lists the deterministic kinds present (empty = none).
	EvidenceKinds []string
	// Confidence is the computed score in [0,1].
	Confidence float64
	// Risk is the assessed action risk.
	Risk RiskLevel
	// Conflicts are confirmed diagnosis contradictions.
	Conflicts int
	// CircuitOpen is true when the repair circuit breaker is open for this
	// failure signature.
	CircuitOpen bool
	// Destructive is true when the action can destroy data.
	Destructive bool
}

// Decide maps the input to a decision plus an operator-readable explanation.
// The ordering of checks IS the policy: the safest verdict wins ties.
func Decide(in DecisionInput) (Decision, string) {
	if in.CircuitOpen {
		return NoAction, "circuit breaker open for this failure signature after repeated failed repairs; autonomous healing paused"
	}
	if in.Destructive || in.Risk >= RiskHigh {
		return RequireApproval, "repair is risky or potentially destructive (" + in.Risk.String() + " risk); user approval required"
	}
	switch in.Transience {
	case TransientFault:
		return NoAction, "observed fault is transient (recovered on re-observation); no modification performed"
	case TransientUnknown:
		if len(in.EvidenceKinds) == 0 {
			return NoAction, "no deterministic evidence available; refusing to modify anything on speculation"
		}
		return Investigate, "evidence insufficient to classify the fault as persistent; collecting deeper diagnostics"
	case EvolvedFault:
		// The failure changed across observations: re-diagnose with fresh
		// evidence rather than acting on the stale picture.
		if in.Conflicts > 0 {
			return Investigate, "fault signature changed across observations and the diagnosis conflicts with evidence; re-investigating"
		}
		return Investigate, "fault signature changed across observations; re-diagnosing with fresh evidence"
	case PersistentFault:
		// Persistent fault: the main decision path.
		if in.Conflicts > 0 {
			return Investigate, fmt.Sprintf("diagnosis conflicts with deterministic evidence (%d contradiction(s)); rejecting diagnosis and re-investigating", in.Conflicts)
		}
		if len(in.EvidenceKinds) == 0 {
			return NoAction, "no deterministic evidence available; refusing to modify anything on speculation"
		}
		if in.Confidence < AutoApplyConfidence {
			if UncertainButActionable(in) {
				return RequireApproval, fmt.Sprintf("diagnosis is actionable but uncertain (confidence %.2f): approval required with explicit uncertainty disclosure", in.Confidence)
			}
			return Investigate, fmt.Sprintf("confidence %.2f below autonomous threshold %.2f; gathering more evidence instead of acting", in.Confidence, AutoApplyConfidence)
		}
		if in.Risk >= RiskMedium {
			return RequireApproval, "diagnosis is well-supported but the repair carries " + in.Risk.String() + " risk; user approval required"
		}
		return AutoApply, "persistent fault, corroborated evidence, no conflicts, and low-risk reversible repair: applying autonomously"
	default:
		return NoAction, "unclassified condition; taking no action"
	}
}

// Verdict is the PreFlight outcome: what the gate instructs the healer to do
// before any repair loop starts.
type Verdict struct {
	Decision   Decision   `json:"decision"`
	Reason     string     `json:"reason"`
	Evidence   Set        `json:"evidence"`
	Transience Transience `json:"transience"`
	// Signature is the dominant problem signature ("" when healthy).
	Signature  string  `json:"signature,omitempty"`
	Confidence float64 `json:"confidence"`
	// BreakerOpen is true when healing is paused for this signature.
	BreakerOpen bool `json:"breaker_open,omitempty"`
}

// Explainable renders the verdict as user-facing text. Non-technical users
// must understand what happened and what ORQEN chose to do without any
// debugging knowledge.
func (v Verdict) Explainable() string {
	var b strings.Builder
	switch v.Decision {
	case NoAction:
		if v.Transience == TransientFault {
			fmt.Fprintf(&b, "The problem resolved on its own (transient fault), so ORQEN changed nothing and will keep watching.\n")
		} else {
			fmt.Fprintf(&b, "ORQEN did not find enough reliable evidence to justify changing your stack, so it changed nothing.\n")
		}
	case Investigate:
		fmt.Fprintf(&b, "ORQEN is still investigating automatically and will collect more diagnostics before touching anything.\n")
	case RequireApproval:
		fmt.Fprintf(&b, "ORQEN found a likely fix, but it is risky, so your approval is required before it runs.\n")
	case AutoApply:
		fmt.Fprintf(&b, "ORQEN verified the problem is persistent and the fix is safe and reversible, so it applied the repair automatically.\n")
	}
	if v.Reason != "" {
		fmt.Fprintf(&b, "Why: %s\n", v.Reason)
	}
	if v.Signature != "" {
		fmt.Fprintf(&b, "Detected problem: %s\n", v.Signature)
	}
	return b.String()
}
