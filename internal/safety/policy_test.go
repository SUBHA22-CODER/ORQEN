package safety

import (
	"strings"
	"testing"
)

func TestDecidePolicyMatrix(t *testing.T) {
	strong := []string{string(KindContainer), string(KindHTTP)}
	cases := []struct {
		name string
		in   DecisionInput
		want Decision
	}{
		{"transient never modifies", DecisionInput{Transience: TransientFault}, NoAction},
		{"circuit open blocks everything", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 1, CircuitOpen: true}, NoAction},
		{"destructive needs approval", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 1, Destructive: true}, RequireApproval},
		{"high risk needs approval", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 1, Risk: RiskHigh}, RequireApproval},
		{"no evidence never acts", DecisionInput{Transience: PersistentFault}, NoAction},
		{"uncertain but actionable needs approval with disclosure", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 0.5}, RequireApproval},
		{"genuinely low confidence investigates", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 0.3}, Investigate},
		{"medium risk needs approval", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 0.9, Risk: RiskMedium}, RequireApproval},
		{"ideal case auto-applies", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 0.9, Risk: RiskLow}, AutoApply},
		{"conflicting diagnosis investigates", DecisionInput{Transience: PersistentFault, EvidenceKinds: strong, Confidence: 0.9, Risk: RiskLow, Conflicts: 1}, Investigate},
		{"unknown transience with evidence investigates", DecisionInput{Transience: TransientUnknown, EvidenceKinds: strong}, Investigate},
	}
	for _, tc := range cases {
		if got, _ := Decide(tc.in); got != tc.want {
			t.Errorf("%s: Decide=%s want %s", tc.name, got, tc.want)
		}
	}
}

func TestDecideExplanationsArePlainLanguage(t *testing.T) {
	_, why := Decide(DecisionInput{Transience: TransientFault})
	if !strings.Contains(why, "transient") || !strings.Contains(why, "no modification") {
		t.Fatalf("transient explanation not user-readable: %s", why)
	}
	_, why = Decide(DecisionInput{Transience: PersistentFault, EvidenceKinds: []string{"container_state"}, Confidence: 0.3})
	if !strings.Contains(why, "confidence") {
		t.Fatalf("low-confidence explanation missing: %s", why)
	}
	_, why = Decide(DecisionInput{CircuitOpen: true})
	if !strings.Contains(why, "circuit breaker") {
		t.Fatalf("circuit explanation missing: %s", why)
	}
}

func TestVerdictExplainable(t *testing.T) {
	v := Verdict{Decision: NoAction, Transience: TransientFault, Reason: "recovered", Signature: "restart_loop:api:exit=1"}
	s := v.Explainable()
	for _, want := range []string{"resolved on its own", "changed nothing", "Why:", "restart_loop"} {
		if !strings.Contains(s, want) {
			t.Errorf("explainable output missing %q:\n%s", want, s)
		}
	}
	v2 := Verdict{Decision: RequireApproval, Reason: "risky"}
	if !strings.Contains(v2.Explainable(), "approval") {
		t.Fatalf("approval verdict not explainable: %s", v2.Explainable())
	}
}
