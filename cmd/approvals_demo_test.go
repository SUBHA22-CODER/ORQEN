package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// withDemoStore isolates demo runs from the user's real approvals.json.
func withDemoStore(t *testing.T) {
	t.Helper()
	prev := demoStoreOverride
	demoStoreOverride = safety.NewApprovalStore("")
	t.Cleanup(func() { demoStoreOverride = prev })
}

func waitForStatus(t *testing.T, store *safety.ApprovalStore, id string, want safety.ApprovalStatus) *safety.Approval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a, ok := store.Get(id); ok && a.Status == want {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %s", want)
	return nil
}

// Demo: low-risk event auto-executes on the isolated fixture with no judge.
func TestDemoLowRiskAutoExecutesOnFixture(t *testing.T) {
	withDemoStore(t)
	report, err := runApprovalDemo(DemoLowRisk)
	if err != nil {
		t.Fatal(err)
	}
	if report["decision"] != "auto_apply" || report["simulated"] != true {
		t.Fatalf("low-risk demo must auto-apply and be labeled simulated: %v", report)
	}
	outcome := report["outcome"].(map[string]any)
	if outcome["executed"] != true {
		t.Fatalf("fixture patch must execute: %v", outcome)
	}
	if pending := demoApprovalStore().List(safety.ApprovalPending, demoProjectID); len(pending) != 0 {
		t.Fatalf("low-risk demo must not create an approval, got %d", len(pending))
	}
}

// Demo: high-risk event pauses for the judge; approval executes the fixture patch.
func TestDemoHighRiskPausesThenJudgeApproves(t *testing.T) {
	withDemoStore(t)
	report, err := runApprovalDemo(DemoHighRisk)
	if err != nil {
		t.Fatal(err)
	}
	if report["decision"] != "awaiting_judge" {
		t.Fatalf("high-risk demo must pause for the judge: %v", report)
	}
	id := report["approval_id"].(string)
	a, ok := demoApprovalStore().Get(id)
	if !ok || a.RiskLevel != "HIGH_RISK" || !a.Simulated {
		t.Fatalf("approval must be simulated HIGH_RISK: %+v", a)
	}
	if _, err := demoApprovalStore().Decide(safety.ApprovalDecision{
		ApprovalID: id, Decision: safety.DecisionRepair, Approved: true,
		ActionHash: a.ActionHash, ProjectID: demoProjectID, DecidedBy: "test-judge",
	}); err != nil {
		t.Fatalf("judge approve failed: %v", err)
	}
	done := waitForStatus(t, demoApprovalStore(), id, safety.ApprovalExecuted)
	if !strings.Contains(done.VerificationResult, "verified") {
		t.Fatalf("execution must record verification: %+v", done)
	}
}

// Demo: uncertain event exposes uncertainty; rejection leaves everything alone.
func TestDemoUncertainApprovalDisclosureAndRejection(t *testing.T) {
	withDemoStore(t)
	report, err := runApprovalDemo(DemoUncertain)
	if err != nil {
		t.Fatal(err)
	}
	id := report["approval_id"].(string)
	a, ok := demoApprovalStore().Get(id)
	if !ok || !a.Uncertain {
		t.Fatalf("uncertain demo must disclose uncertainty: %+v", a)
	}
	if len(a.Evidence) == 0 || a.ExpectedImpact == "" || a.RollbackPlan == "" {
		t.Fatalf("uncertain approval must carry evidence, impact, rollback: %+v", a)
	}
	if _, err := demoApprovalStore().Decide(safety.ApprovalDecision{
		ApprovalID: id, Approved: false, ActionHash: a.ActionHash,
		ProjectID: demoProjectID, Reason: "evidence insufficient",
	}); err != nil {
		t.Fatalf("judge reject failed: %v", err)
	}
	got := waitForStatus(t, demoApprovalStore(), id, safety.ApprovalRejected)
	if got.RejectionReason != "evidence insufficient" {
		t.Fatalf("rejection reason must be recorded: %+v", got)
	}
}

// Demo: destructive event is refused by policy — approval never bypasses
// prohibitions, and the lifecycle records the refusal honestly.
func TestDemoDestructiveRefusedEvenWithLifecycle(t *testing.T) {
	withDemoStore(t)
	report, err := runApprovalDemo(DemoDestructive)
	if err != nil {
		t.Fatal(err)
	}
	if report["decision"] != "refused_prohibited" {
		t.Fatalf("destructive demo must be refused by policy: %v", report)
	}
	id := report["approval_id"].(string)
	a := waitForStatus(t, demoApprovalStore(), id, safety.ApprovalFailed)
	if !strings.Contains(a.VerificationResult, "prohibited") {
		t.Fatalf("refusal must be recorded: %+v", a)
	}
	if !strings.Contains(a.RollbackResult, "no modification was ever applied") {
		t.Fatalf("rollback must state nothing was applied: %+v", a)
	}
}
