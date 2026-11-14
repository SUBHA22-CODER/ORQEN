package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/config"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// withCLIStore isolates approvals CLI runs from the real approvals.json.
func withCLIStore(t *testing.T) *safety.ApprovalStore {
	t.Helper()
	prev := approvalStoreOverride
	store := safety.NewApprovalStore("")
	approvalStoreOverride = store
	t.Cleanup(func() { approvalStoreOverride = prev })
	return store
}

func newCLIApproval(t *testing.T, store *safety.ApprovalStore) *safety.Approval {
	t.Helper()
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n"}
	a, err := store.Create(safety.Approval{
		ProjectID:        "proj-1",
		ActionType:       "manifest_patch",
		ActionSummary:    "replace compose with a patched version",
		RiskLevel:        "HIGH_RISK",
		AffectedFiles:    []string{"docker-compose.yml"},
		AffectedServices: []string{"api"},
		ActionHash:       safety.HashAction("manifest_patch", files),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApprovalsCLIRoundTrip(t *testing.T) {
	store := withCLIStore(t)
	a := newCLIApproval(t, store)

	// list shows the pending request.
	if err := runApprovalsList(""); err != nil {
		t.Fatalf("list: %v", err)
	}

	// show returns full details without error.
	if err := runApprovalsShow(a.ApprovalID, false); err != nil {
		t.Fatalf("show: %v", err)
	}

	// approve binds to the stored action hash and succeeds once.
	if err := runApprovalsDecide(a.ApprovalID, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalApproved || got.Decision != safety.DecisionRepair || !strings.Contains(got.DecidedBy, "(cli)") {
		t.Fatalf("approval must be APPROVED by the cli identity: %+v", got)
	}

	// Approval enforcement: a second decision is refused.
	if err := runApprovalsDecide(a.ApprovalID, true); err == nil {
		t.Fatal("re-approving an already-decided approval must fail")
	}
	if err := runApprovalsShow(a.ApprovalID, false); err != nil {
		t.Fatalf("show after decision: %v", err)
	}

	// reject on a fresh request records the ruling.
	b := newCLIApproval(t, store)
	if err := runApprovalsDecide(b.ApprovalID, false); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got2, _ := store.Get(b.ApprovalID)
	if got2.Status != safety.ApprovalRejected {
		t.Fatalf("approval must be REJECTED: %+v", got2)
	}
}

func TestApprovalsCLIRejectsUnknownAndHashMismatch(t *testing.T) {
	store := withCLIStore(t)
	if err := runApprovalsShow("apr-does-not-exist", false); err == nil {
		t.Fatal("show of unknown id must fail")
	}
	if err := runApprovalsDecide("apr-does-not-exist", true); err == nil {
		t.Fatal("approve of unknown id must fail")
	}

	// A decision whose hash does not match the record is refused even though
	// the id exists: authorization is bound to the exact action.
	a := newCLIApproval(t, store)
	d := safety.ApprovalDecision{
		ApprovalID: a.ApprovalID, Approved: true,
		ActionHash: "deadbeef", ProjectID: a.ProjectID, DecidedBy: "cli",
	}
	if _, err := store.Decide(d); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("hash mismatch must be refused with an explanation, got: %v", err)
	}
}

func TestSafetyTestCommandRuns(t *testing.T) {
	// All three scenarios pass through the command runner.
	summary := safety.RunSafetyScenarios()
	if summary.Passed != 3 || summary.Failed != 0 || summary.Skipped != 0 {
		t.Fatalf("expected 3 passed / 0 failed / 0 skipped, got %d/%d/%d",
			summary.Passed, summary.Failed, summary.Skipped)
	}
	// A single named scenario runs alone.
	one := safety.RunSafetyScenarios("safe")
	if one.Passed != 1 || len(one.Results) != 1 || one.Results[0].Name != "safe" {
		t.Fatalf("single-scenario run wrong: %+v", one)
	}
}

// TestApprovalsCLIIdentityVerification: the CLI approval command must verify
// the current user's authorization — a viewer is refused, a judge succeeds.
func TestApprovalsCLIIdentityVerification(t *testing.T) {
	t.Setenv("ORQEN_HOME", t.TempDir()) // hermetic judges roster
	store := withCLIStore(t)
	a := newCLIApproval(t, store)

	// Seed the roster: after first-run bootstrap, deployments have an
	// explicit judge list, and anyone absent from it is a viewer.
	home0, _ := config.Home()
	roster := []byte(`{"judges":[{"user":"test-judge","role":"judge"}]}`)
	if err := os.WriteFile(filepath.Join(home0, "judges.json"), roster, 0o600); err != nil {
		t.Fatal(err)
	}

	// A user absent from the roster resolves to viewer: refused, no change.
	approvalsAsUser = "random-viewer"
	t.Cleanup(func() { approvalsAsUser = "" })
	if err := runApprovalsDecide(a.ApprovalID, true); err == nil || !strings.Contains(err.Error(), "only judge or reviewer") {
		t.Fatalf("unauthorized CLI user must be refused, got: %v", err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalPending {
		t.Fatal("unauthorized CLI attempt must not change state")
	}

	// The seeded judge approves.
	approvalsAsUser = "test-judge"
	if err := runApprovalsDecide(a.ApprovalID, true); err != nil {
		t.Fatalf("authorized CLI approve must succeed: %v", err)
	}
	got, _ = store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalApproved || !strings.Contains(got.DecidedBy, "test-judge") || !strings.Contains(got.DecidedBy, "(cli)") {
		t.Fatalf("decision must be recorded by the cli identity: %+v", got)
	}
}
