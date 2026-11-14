package safety

import (
	"fmt"
	"strings"
)

// RiskLabel renders a machine-facing risk classification for approvals and
// audit records: LOW_RISK, MEDIUM_RISK, HIGH_RISK, or CRITICAL_RISK.
func (r RiskLevel) RiskLabel() string {
	return strings.ToUpper(r.String()) + "_RISK"
}

// ConfidenceTier classifies a confidence score in [0,1] independently of the
// risk assessment: HIGH_CONFIDENCE (>= 0.7), MEDIUM_CONFIDENCE (>= 0.45),
// LOW_CONFIDENCE otherwise. The tiers deliberately mirror AutoApplyConfidence
// so HIGH_CONFIDENCE and "autonomous" mean the same thing.
func ConfidenceTier(score float64) string {
	switch {
	case score >= AutoApplyConfidence:
		return "HIGH_CONFIDENCE"
	case score >= 0.45:
		return "MEDIUM_CONFIDENCE"
	default:
		return "LOW_CONFIDENCE"
	}
}

// Prohibited reports whether a proposed patch content must be refused
// outright — even with an explicit approval. These are policy hard limits,
// distinct from risky-but-reviewable actions: data destruction, security
// downgrades, and health-check removals can never be authorized by a judge.
func Prohibited(content string) []string {
	low := strings.ToLower(content)
	var out []string
	for _, p := range []struct {
		marker string
		why    string
	}{
		{"drop table", "deletes database tables"},
		{"drop database", "deletes a database"},
		{"truncate table", "wipes table data"},
		{"docker volume rm", "destroys named volumes"},
		{"down -v", "destroys all stack volumes"},
		{"docker system prune", "destroys unused docker data stack-wide"},
		{"rm -rf /", "recursive deletion at filesystem root"},
		{"no_verify_ssl", "disables TLS verification"},
		{"insecure_skip_verify", "disables TLS verification"},
		{"sslverify false", "disables git TLS verification"},
	} {
		if strings.Contains(low, p.marker) {
			out = append(out, fmt.Sprintf("%s (%s)", p.marker, p.why))
		}
	}
	return out
}

// UncertainButActionable reports whether a diagnosis is actionable (some
// deterministic evidence exists, no conflicts, risk is reviewable) while the
// confidence is only medium: the policy answer is an approval request that
// explicitly discloses the uncertainty, rather than silent auto-execution or
// an endless investigation loop.
func UncertainButActionable(in DecisionInput) bool {
	if in.Conflicts > 0 || in.Destructive || in.CircuitOpen {
		return false
	}
	if len(in.EvidenceKinds) == 0 {
		return false
	}
	if in.Risk >= RiskHigh {
		return false
	}
	return in.Confidence >= 0.45 && in.Confidence < AutoApplyConfidence
}
