package safety

import (
	"strings"
	"testing"
)

// --- score boundaries and thresholds ----------------------------------------

func TestConfidenceTierThresholds(t *testing.T) {
	// Tier bands: HIGH >= 0.70, MEDIUM >= 0.45, LOW otherwise. Boundaries are
	// inclusive on the upper edge of each band.
	if ConfidenceTier(0.70) != "HIGH_CONFIDENCE" || ConfidenceTier(0.6999) != "MEDIUM_CONFIDENCE" {
		t.Fatal("HIGH/MEDIUM boundary must be exactly 0.70")
	}
	if ConfidenceTier(0.45) != "MEDIUM_CONFIDENCE" || ConfidenceTier(0.4499) != "LOW_CONFIDENCE" {
		t.Fatal("MEDIUM/LOW boundary must be exactly 0.45")
	}
	if ConfidenceTier(0) != "LOW_CONFIDENCE" || ConfidenceTier(1) != "HIGH_CONFIDENCE" {
		t.Fatal("extreme scores misclassified")
	}
}

func TestRiskLevelFromScoreBoundaries(t *testing.T) {
	cases := []struct {
		score int
		want  RiskLevel
	}{
		{0, RiskLow}, {39, RiskLow}, {40, RiskMedium}, {59, RiskMedium},
		{60, RiskHigh}, {84, RiskHigh}, {85, RiskCritical}, {100, RiskCritical},
	}
	for _, tc := range cases {
		if got := levelFromScore(tc.score); got != tc.want {
			t.Errorf("levelFromScore(%d)=%s want %s", tc.score, got, tc.want)
		}
	}
}

func TestRiskScoreFactorsDeterministic(t *testing.T) {
	in := RiskInput{
		Action:          ActionMultiManifest,
		FilesTouched:    []string{"a", "b", "c"},
		ServicesTouched: []string{"api", "web"},
		Reversible:      true,
	}
	score, factors := RiskScore(in)
	// base 50 + services 15 + files 15 = 80
	if score != 80 {
		t.Fatalf("risk score = %d, want 80", score)
	}
	if len(factors) != 4 { // base + services + files + reversible note
		t.Fatalf("expected 4 factor explanations, got %d: %v", len(factors), factors)
	}
	// Same inputs, same outputs: determinism.
	score2, factors2 := RiskScore(in)
	if score2 != score || len(factors2) != len(factors) {
		t.Fatal("RiskScore must be deterministic")
	}
}

func TestRiskScoreDestructiveForcedTo100(t *testing.T) {
	score, factors := RiskScore(RiskInput{
		Action:             ActionManifestPatch,
		Reversible:         true,
		DestructiveMarkers: []string{"down -v"},
	})
	if score != 100 {
		t.Fatalf("destructive markers must force risk to 100, got %d", score)
	}
	if len(factors) == 0 {
		t.Fatal("destructive risk must explain its factors")
	}
}

// --- evidence sufficiency ----------------------------------------------------

func TestSufficiencyTiersAndEmptyEvidence(t *testing.T) {
	if got, _ := EvidenceSufficiency(Set{}, 0); got != 0 {
		t.Fatalf("empty evidence sufficiency = %d, want 0", got)
	}
	ev := scenarioEvidence("container", "http")
	got, factors := EvidenceSufficiency(ev, 3)
	if got < 70 {
		t.Fatalf("two corroborating kinds + 3 reps must be SUFFICIENT, got %d", got)
	}
	if SufficiencyTier(got) != "SUFFICIENT" {
		t.Fatalf("tier wrong: %s", SufficiencyTier(got))
	}
	if len(factors) == 0 {
		t.Fatal("sufficiency must explain its factors")
	}
	// Single weak observation: partial at best.
	got2, _ := EvidenceSufficiency(scenarioEvidence("log"), 1)
	if SufficiencyTier(got2) == "SUFFICIENT" {
		t.Fatalf("one weak signal with one repetition must not be SUFFICIENT, got %d", got2)
	}
}

// --- conflicting evidence ----------------------------------------------------

func TestConflictingEvidenceLowersConfidence(t *testing.T) {
	// Two agreeing problem signals vs one problem + one healthy observation:
	// the disagreement must lower confidence.
	agree := scenarioEvidence("container", "http")
	conflicting := scenarioEvidence("container")
	conflicting.Add(Evidence{
		ID: "http:api", Kind: KindHTTP, Source: SourceProbe, Service: "api",
		Healthy: true, Detail: "healthy (code=200)",
	})
	high := AssessEvidence(agree, 3, RiskInput{Action: ActionManifestPatch, Reversible: true, ServicesTouched: []string{"api"}})
	low := AssessEvidence(conflicting, 3, RiskInput{Action: ActionManifestPatch, Reversible: true, ServicesTouched: []string{"api"}})
	if low.Confidence >= high.Confidence {
		t.Fatalf("conflicting evidence must lower confidence: agree=%d conflict=%d", high.Confidence, low.Confidence)
	}
	// And the factor list must explain the disagreement.
	found := false
	for _, f := range low.ConfidenceFactors {
		if strings.Contains(f, "agreement: 1 of 2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflict factor explanation missing: %v", low.ConfidenceFactors)
	}
}

// --- confidence and risk are independent measures ----------------------------

func TestConfidenceAndRiskAreIndependent(t *testing.T) {
	ev := scenarioEvidence("container", "http")
	// Same evidence, two different actions: confidence identical, risk differs.
	lowRisk := AssessEvidence(ev, 3, RiskInput{Action: ActionManifestPatch, Reversible: true, ServicesTouched: []string{"api"}})
	highRisk := AssessEvidence(ev, 3, RiskInput{Action: ActionMultiManifest,
		FilesTouched: []string{"a", "b", "c"}, ServicesTouched: []string{"api", "web"}, Reversible: true})
	if lowRisk.Confidence != highRisk.Confidence {
		t.Fatal("confidence must not change with the proposed action")
	}
	if highRisk.Risk <= lowRisk.Risk {
		t.Fatal("risk must grow with the blast radius of the action")
	}
	if highRisk.Decision != RequireApproval {
		t.Fatalf("high risk must require approval even at %d confidence", highRisk.Confidence)
	}
}

// --- high risk requires approval regardless of confidence --------------------

func TestHighRiskRequiresApprovalRegardlessOfConfidence(t *testing.T) {
	risky := RiskInput{Action: ActionMultiManifest,
		FilesTouched: []string{"a", "b", "c"}, ServicesTouched: []string{"api", "web"}, Reversible: true}
	strong := AssessEvidence(scenarioEvidence("container", "http"), 3, risky)
	weak := AssessEvidence(scenarioEvidence("log"), 1, risky)
	if strong.Confidence < 70 {
		t.Fatalf("fixture sanity: strong confidence=%d", strong.Confidence)
	}
	if weak.Confidence >= 70 {
		t.Fatalf("fixture sanity: weak confidence=%d", weak.Confidence)
	}
	if strong.Decision != RequireApproval || weak.Decision != RequireApproval {
		t.Fatalf("high risk must require approval regardless of confidence: strong=%s weak=%s",
			strong.Decision, weak.Decision)
	}
}

// --- insufficient evidence never auto-modifies --------------------------------

func TestInsufficientEvidenceNeverAutomatic(t *testing.T) {
	for _, risk := range []RiskInput{
		{Action: ActionManifestPatch, Reversible: true, ServicesTouched: []string{"api"}},
		{Action: ActionDataDestructive},
	} {
		rep := AssessEvidence(Set{}, 0, risk)
		if rep.Decision == AutoApply {
			t.Fatalf("empty evidence must never auto-apply (risk %+v)", risk)
		}
		if rep.Confidence != 0 || rep.Sufficiency != 0 {
			t.Fatalf("no evidence must score zero: conf=%d suff=%d", rep.Confidence, rep.Sufficiency)
		}
		if len(rep.ConfidenceFactors) == 0 || len(rep.SufficiencyFactors) == 0 {
			t.Fatal("empty evidence must still explain itself")
		}
	}
}

// --- deterministic scenarios ---------------------------------------------------

func TestSafetyScenariosAllPass(t *testing.T) {
	summary := RunSafetyScenarios()
	if summary.Passed != 3 || summary.Failed != 0 || summary.Skipped != 0 {
		t.Fatalf("scenarios: passed=%d failed=%d skipped=%d", summary.Passed, summary.Failed, summary.Skipped)
	}
	for _, r := range summary.Results {
		if r.Status != "PASS" {
			t.Fatalf("scenario %s: %s (%s)", r.Name, r.Status, r.Detail)
		}
	}
	// Expected decision mapping per the spec.
	safe := RunSafetyScenario(SafetyScenarios()["safe"])
	if safe.Report.Decision != AutoApply || safe.Report.Confidence < 70 || safe.Report.Risk >= 40 || safe.Report.Sufficiency < 70 {
		t.Fatalf("safe scenario expectations violated: %+v", safe.Report)
	}
	high := RunSafetyScenario(SafetyScenarios()["high-risk"])
	if high.Report.Decision != RequireApproval {
		t.Fatalf("high-risk scenario must require approval: %+v", high.Report)
	}
	ins := RunSafetyScenario(SafetyScenarios()["insufficient-evidence"])
	if ins.Report.Decision != NoAction {
		t.Fatalf("insufficient evidence must block: %+v", ins.Report)
	}
}

func TestSafetyScenariosDeterministicAndUnknownSkipped(t *testing.T) {
	first := RunSafetyScenarios("safe", "high-risk", "insufficient-evidence")
	second := RunSafetyScenarios("safe", "high-risk", "insufficient-evidence")
	for i := range first.Results {
		if first.Results[i].Report.Confidence != second.Results[i].Report.Confidence ||
			first.Results[i].Report.Risk != second.Results[i].Report.Risk ||
			first.Results[i].Report.Sufficiency != second.Results[i].Report.Sufficiency ||
			first.Results[i].Report.Decision != second.Results[i].Report.Decision {
			t.Fatalf("scenario %s is not deterministic", first.Results[i].Name)
		}
	}
	// Unknown scenario names are counted SKIPPED, never FAIL.
	summary := RunSafetyScenarios("safe", "bogus")
	if summary.Skipped != 1 || summary.Passed != 1 {
		t.Fatalf("unknown scenario must be skipped: %+v", summary)
	}
}
