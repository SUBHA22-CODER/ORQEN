package safety

import (
	"testing"
)

func TestScoreNoEvidenceIsZero(t *testing.T) {
	if s := Score(ConfidenceInput{Evidence: Set{}}); s != 0 {
		t.Fatalf("no deterministic evidence must score 0, got %.2f", s)
	}
}

func TestScoreModelOnlyCapped(t *testing.T) {
	// Evidence exists but carries no problem signatures: the diagnosis rests
	// on model assertions alone, so confidence must stay low.
	ev := evidenceSet(Evidence{
		ID: "container:api", Kind: KindContainer, Source: SourceDocker,
		Service: "api", Healthy: true,
	})
	s := Score(ConfidenceInput{Evidence: ev, ModelOnly: true, Repetitions: 3})
	if s > llmOnlyCap {
		t.Fatalf("model-only confidence %.2f exceeds cap %.2f", s, llmOnlyCap)
	}
	if s > AutoApplyConfidence {
		t.Fatalf("model-only confidence must never reach the autonomous threshold")
	}
}

func TestScoreStrongEvidenceHigh(t *testing.T) {
	// Persistent fault seen by container state + http + log: high quality,
	// full reproducibility, full agreement.
	ev := evidenceSet(
		Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"},
		Evidence{ID: "http:api", Kind: KindHTTP, Source: SourceProbe, Service: "api", Signature: "http_timeout:api"},
		Evidence{ID: "log:api", Kind: KindLog, Source: SourceLog, Service: "api", Signature: "log_exit_1:api"},
	)
	s := Score(ConfidenceInput{Evidence: ev, Repetitions: 3, SignalsAgree: 3, SignalsTotal: 3})
	if s < AutoApplyConfidence {
		t.Fatalf("corroborated persistent fault should exceed autonomous threshold, got %.2f", s)
	}
}

func TestScoreConflictsReduce(t *testing.T) {
	ev := evidenceSet(
		Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"},
	)
	base := Score(ConfidenceInput{Evidence: ev, Repetitions: 3, SignalsAgree: 1, SignalsTotal: 1})
	withConflict := Score(ConfidenceInput{Evidence: ev, Repetitions: 3, SignalsAgree: 1, SignalsTotal: 1, Conflicts: 2})
	if withConflict >= base {
		t.Fatalf("conflicts must reduce confidence: base %.2f conflicted %.2f", base, withConflict)
	}
}

func TestScoreSingleObservationBelowThreshold(t *testing.T) {
	// One observation of a strong signal: quality 0.9 * (0.5 + 0.1) = 0.54,
	// below the 0.7 autonomous threshold — one look is never enough.
	ev := evidenceSet(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "x"})
	s := Score(ConfidenceInput{Evidence: ev, Repetitions: 1, SignalsAgree: 1, SignalsTotal: 1})
	if s >= AutoApplyConfidence {
		t.Fatalf("single observation must not auto-apply, got %.2f", s)
	}
}

func TestScoreBounded(t *testing.T) {
	ev := evidenceSet(Evidence{ID: "c", Kind: KindContainer, Source: SourceDocker, Signature: "s"})
	if s := Score(ConfidenceInput{Evidence: ev, Repetitions: 100, SignalsAgree: 10, SignalsTotal: 1, Conflicts: 99}); s < 0 {
		t.Fatalf("negative score: %.2f", s)
	}
}

func TestAssessRiskLevels(t *testing.T) {
	cases := []struct {
		in   RiskInput
		want RiskLevel
	}{
		{RiskInput{Action: ActionRestart, Reversible: true}, RiskLow},
		{RiskInput{Action: ActionManifestPatch, FilesTouched: []string{"Dockerfile.api"}, Reversible: true}, RiskLow},
		{RiskInput{Action: ActionManifestPatch, Reversible: false}, RiskHigh},
		{RiskInput{Action: ActionMultiManifest, FilesTouched: []string{"a", "b"}, Reversible: true}, RiskMedium},
		{RiskInput{Action: ActionMultiManifest, FilesTouched: []string{"a", "b"}, ServicesTouched: []string{"api", "web"}, Reversible: true}, RiskHigh},
		{RiskInput{Action: ActionInfraChange, Reversible: false}, RiskHigh},
		{RiskInput{Action: ActionManifestPatch, Reversible: true, DestructiveMarkers: []string{"down -v"}}, RiskCritical},
	}
	for _, tc := range cases {
		if got := Assess(tc.in); got != tc.want {
			t.Errorf("Assess(%+v)=%s want %s", tc.in, got, tc.want)
		}
	}
}

func TestDetectDestructiveMarkers(t *testing.T) {
	got := DetectDestructiveMarkers("services:\n  db:\n    # docker volume rm all\n    command: drop database")
	if len(got) == 0 {
		t.Fatal("expected destructive markers")
	}
	if clean := DetectDestructiveMarkers("services:\n  api:\n    image: node:22\n"); len(clean) != 0 {
		t.Fatalf("clean patch flagged: %v", clean)
	}
}

func TestExplainRisk(t *testing.T) {
	out := Explain(RiskInput{Action: ActionDataDestructive, Reversible: false}, RiskCritical)
	if out == "" || out == "critical risk" {
		t.Fatalf("expected detailed explanation, got %q", out)
	}
}
