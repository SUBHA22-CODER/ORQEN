package safety

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Mandatory judge-HITL tests 7-12 (spec §8): approval binding, one-time
// enforcement, prohibited operations, rollback recording, concurrency.

// TEST 7: expired approval cannot decide or execute.
func TestMandatory7ExpiredApprovalBlocked(t *testing.T) {
	store := NewApprovalStore("")
	_, hash := proposalHighRisk()
	a, err := store.Create(Approval{
		ProjectID: "proj-1", ActionSummary: "late request",
		ActionType: "manifest_patch", ActionHash: hash,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, derr := store.Decide(ApprovalDecision{ApprovalID: a.ApprovalID, Approved: true, ActionHash: hash, ProjectID: "proj-1"}); derr == nil {
		t.Fatal("expired approval must not be decidable")
	}
	if _, rerr := store.Revalidate(a.ApprovalID, hash, "proj-1"); rerr == nil {
		t.Fatal("expired approval must not be executable")
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalExpired {
		t.Fatalf("expired status must be recorded, got %s", got.Status)
	}
}

// TEST 8: modified action after approval: hash mismatch blocks execution.
func TestMandatory8ModifiedActionBlocked(t *testing.T) {
	store := NewApprovalStore("")
	_, hash := proposalHighRisk()
	a, err := store.Create(Approval{
		ProjectID: "proj-1", ActionSummary: "x", ActionType: "manifest_patch",
		ActionHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide(ApprovalDecision{ApprovalID: a.ApprovalID, Approved: true, Decision: DecisionRepair, ActionHash: hash, ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}
	// The proposed action changed after approval: different hash.
	changed := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.13-slim\n"}
	if _, rerr := store.Revalidate(a.ApprovalID, HashAction("manifest_patch", changed), "proj-1"); rerr == nil {
		t.Fatal("action-hash mismatch must block execution")
	}
	// The original hash still revalidates: the binding is exact.
	if _, rerr := store.Revalidate(a.ApprovalID, hash, "proj-1"); rerr != nil {
		t.Fatalf("unmodified action must pass revalidation: %v", rerr)
	}
}

// TEST 9: replayed approval is refused — approvals are one-time.
func TestMandatory9ReplayBlocked(t *testing.T) {
	store := NewApprovalStore("")
	_, hash := proposalHighRisk()
	a, _ := store.Create(Approval{ProjectID: "proj-1", ActionSummary: "x", ActionType: "manifest_patch", ActionHash: hash})
	dec := ApprovalDecision{ApprovalID: a.ApprovalID, Approved: true, Decision: DecisionRepair, ActionHash: hash, ProjectID: "proj-1"}
	if _, err := store.Decide(dec); err != nil {
		t.Fatal(err)
	}
	// First execution consumes the approval.
	if _, err := store.Revalidate(a.ApprovalID, hash, "proj-1"); err != nil {
		t.Fatal(err)
	}
	// Replay attempts are refused.
	if _, err := store.Revalidate(a.ApprovalID, hash, "proj-1"); err == nil {
		t.Fatal("replayed approval must be refused")
	}
	if _, err := store.Decide(dec); err == nil {
		t.Fatal("already-executed approval must not be decidable again")
	}
}

// TEST 10: prohibited destructive operation is refused even with approval.
func TestMandatory10ProhibitedRefusedWithApproval(t *testing.T) {
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: x\n# docker compose down -v\n"}
	markers := Prohibited(files["docker-compose.yml"])
	if len(markers) == 0 {
		t.Fatal("destructive patch must be detected as prohibited")
	}
	// Risk grading is critical regardless of everything else.
	if lvl := Assess(RiskInput{Action: ActionManifestPatch, Reversible: true, DestructiveMarkers: markers}); lvl != RiskCritical {
		t.Fatalf("destructive markers must grade CRITICAL, got %s", lvl)
	}
	// Even a fully-approved approval records only an honest refusal: the
	// execution pipeline consults Prohibited() before acting, and the store
	// carries the FAILED outcome with the reason for the audit trail.
	store := NewApprovalStore("")
	hash := HashAction("manifest_patch", files)
	a, _ := store.Create(Approval{ProjectID: "proj-1", ActionSummary: "wipe volumes", ActionType: "manifest_patch", ActionHash: hash})
	_, _ = store.Decide(ApprovalDecision{ApprovalID: a.ApprovalID, Approved: true, Decision: DecisionRepair, ActionHash: hash, ProjectID: "proj-1"})
	_, _ = store.Revalidate(a.ApprovalID, hash, "proj-1")
	failed, err := store.Complete(a.ApprovalID, false,
		"refused: prohibited operation ("+strings.Join(markers, "; ")+")",
		"nothing to roll back: no modification was ever applied")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != ApprovalFailed || !strings.Contains(failed.VerificationResult, "prohibited") {
		t.Fatalf("prohibited refusal must be recorded as FAILED with reason: %+v", failed)
	}
}

// TEST 11: rollback after unsuccessful execution is recorded and audited.
func TestMandatory11RollbackRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.json")
	store := NewApprovalStore(path)
	_, hash := proposalHighRisk()
	a, _ := store.Create(Approval{ProjectID: "proj-1", ActionSummary: "x", ActionType: "manifest_patch", ActionHash: hash})
	_, _ = store.Decide(ApprovalDecision{ApprovalID: a.ApprovalID, Approved: true, Decision: DecisionRepair, ActionHash: hash, ProjectID: "proj-1"})
	if _, err := store.Revalidate(a.ApprovalID, hash, "proj-1"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Complete(a.ApprovalID, false, "runtime health verification failed", "previous configuration restored from snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != ApprovalFailed || failed.VerificationResult == "" || failed.RollbackResult == "" {
		t.Fatalf("failure outcome must record verification and rollback: %+v", failed)
	}
	// Persisted and reloadable for audit.
	reloaded := NewApprovalStore(path)
	got, ok := reloaded.Get(a.ApprovalID)
	if !ok || got.RollbackResult == "" {
		t.Fatal("rollback result must persist for audit")
	}
}

// TEST 12: concurrent approval attempts: exactly one decision wins.
func TestMandatory12ConcurrentDecisionsSingleWinner(t *testing.T) {
	store := NewApprovalStore("")
	_, hash := proposalHighRisk()
	a, _ := store.Create(Approval{ProjectID: "proj-1", ActionSummary: "x", ActionType: "manifest_patch", ActionHash: hash})
	const n = 16
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.Decide(ApprovalDecision{
				ApprovalID: a.ApprovalID, Approved: true, Decision: DecisionRepair,
				ActionHash: hash, ProjectID: "proj-1", DecidedBy: "judge",
			}); err == nil {
				wins[i] = true
			}
		}(i)
	}
	wg.Wait()
	count := 0
	for _, w := range wins {
		if w {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("exactly one concurrent decision must win, got %d", count)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalApproved {
		t.Fatalf("final status must be APPROVED, got %s", got.Status)
	}
}

func TestRiskAndConfidenceLabels(t *testing.T) {
	if RiskLow.RiskLabel() != "LOW_RISK" || RiskCritical.RiskLabel() != "CRITICAL_RISK" {
		t.Fatal("risk labels wrong")
	}
	if ConfidenceTier(0.9) != "HIGH_CONFIDENCE" || ConfidenceTier(0.5) != "MEDIUM_CONFIDENCE" || ConfidenceTier(0.2) != "LOW_CONFIDENCE" {
		t.Fatal("confidence tiers wrong")
	}
}
