package safety

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Mandatory judge-HITL tests 1-6 (spec §8). Tests 7-12 live in
// approval_lifecycle_test.go. All are deterministic: fakes only, no Docker.

func validEvidence() Set {
	ev := Set{}
	ev.Add(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"})
	ev.Add(Evidence{ID: "log:api", Kind: KindLog, Source: SourceLog, Service: "api", Signature: "log_exit_1:api"})
	return ev
}

func highRiskVerdict() Verdict {
	return Verdict{Evidence: validEvidence(), Transience: PersistentFault, Signature: "restart_loop:api:exit=1"}
}

func proposalHighRisk() (Proposal, string) {
	// Three files across two services: the real risk engine grades this
	// ActionMultiManifest with a HIGH_RISK blast radius, which the policy
	// routes to RequireApproval.
	files := map[string]string{
		"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n  web:\n    image: node:22\n",
		"Dockerfile.api":     "FROM python:3.12-slim\n",
		"Dockerfile.web":     "FROM node:22\n",
	}
	return Proposal{Files: files, Service: "api", Summary: "recreate api and web"}, HashAction("multi_manifest", files)
}

func newTestGate(store *ApprovalStore) *Gate {
	return &Gate{
		Validator:   Validator{},
		Sandbox:     &Sandbox{Runner: &fakeSandboxRunner{}},
		Coordinator: &ApprovalCoordinator{Store: store, ProjectID: "proj-1"},
	}
}

// waitForPending polls until a PENDING approval exists, returning it.
func waitForPending(t *testing.T, store *ApprovalStore, projectID string) *Approval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := store.List(ApprovalPending, projectID); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a pending approval")
	return nil
}

// TEST 1: low-risk action auto-executes without judge approval.
func TestMandatory1LowRiskAutoExecutes(t *testing.T) {
	store := NewApprovalStore("")
	g := newTestGate(store)
	files := map[string]string{"Dockerfile.api": "FROM python:3.11-slim\nRUN pip install .\n"}
	verdict := Verdict{Evidence: validEvidence(), Transience: PersistentFault, Signature: "s1"}
	pd := g.ReviewPlan(context.Background(), verdict, Diagnosis{Raw: "lockfile desync", SourceKind: "llm"}, Proposal{Files: files, Service: "api"})
	if pd.Decision != AutoApply {
		t.Fatalf("low-risk verified fix must auto-apply, got %s (%s)", pd.Decision, pd.Explanation)
	}
	if pending := store.List(ApprovalPending, ""); len(pending) != 0 {
		t.Fatalf("no approval may be created for a low-risk verified fix, got %d", len(pending))
	}
}

// TEST 2: high-risk action pauses, judge approves, validated action executes.
func TestMandatory2HighRiskPauseApproveExecute(t *testing.T) {
	store := NewApprovalStore("")
	g := newTestGate(store)
	p, hash := proposalHighRisk()
	done := make(chan PlanDecision, 1)
	go func() {
		done <- g.ReviewPlan(context.Background(), highRiskVerdict(), Diagnosis{Raw: "recreate service", SourceKind: "llm"}, p)
	}()
	a := waitForPending(t, store, "proj-1")
	if a.RiskLevel != "HIGH_RISK" || a.ActionHash != hash {
		t.Fatalf("approval binding wrong: risk=%s hash-match=%v", a.RiskLevel, a.ActionHash == hash)
	}
	if _, err := store.Decide(ApprovalDecision{
		ApprovalID: a.ApprovalID, Decision: DecisionRepair, Approved: true,
		ActionHash: hash, ProjectID: "proj-1", DecidedBy: "judge",
	}); err != nil {
		t.Fatalf("judge approve failed: %v", err)
	}
	pd := <-done
	if pd.Decision != RequireApproval {
		t.Fatalf("decision should remain approval-gated, got %s", pd.Decision)
	}
	if !strings.Contains(pd.Explanation, "approved") {
		t.Fatalf("approved explanation missing: %s", pd.Explanation)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalExecuting {
		t.Fatalf("approved+revalidated approval must be EXECUTING, got %s", got.Status)
	}
}

// TEST 3: judge rejects the high-risk action: no modification occurs.
func TestMandatory3RejectionBlocksExecution(t *testing.T) {
	store := NewApprovalStore("")
	g := newTestGate(store)
	p, hash := proposalHighRisk()
	done := make(chan PlanDecision, 1)
	go func() {
		done <- g.ReviewPlan(context.Background(), highRiskVerdict(), Diagnosis{Raw: "", SourceKind: "llm"}, p)
	}()
	a := waitForPending(t, store, "proj-1")
	if _, err := store.Decide(ApprovalDecision{
		ApprovalID: a.ApprovalID, Approved: false, ActionHash: hash,
		ProjectID: "proj-1", DecidedBy: "judge", Reason: "not safe",
	}); err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	pd := <-done
	if pd.Decision != NoAction {
		t.Fatalf("rejected repair must be NoAction, got %s", pd.Decision)
	}
	if !strings.Contains(pd.Explanation, "not approved") {
		t.Fatalf("explanation must state nothing was modified: %s", pd.Explanation)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalRejected || got.RejectionReason != "not safe" {
		t.Fatalf("rejection must be recorded: %+v", got)
	}
}

// TEST 4: uncertain root cause → additional autonomous investigation.
func TestMandatory4UncertainDiagnosisInvestigates(t *testing.T) {
	in := DecisionInput{
		Transience:    PersistentFault,
		EvidenceKinds: []string{string(KindLog)},
		Confidence:    0.3,
		Risk:          RiskLow,
	}
	d, why := Decide(in)
	if d != Investigate {
		t.Fatalf("uncertain root cause must trigger investigation, got %s (%s)", d, why)
	}
	if UncertainButActionable(in) {
		t.Fatal("very low confidence must not be classified uncertain-but-actionable")
	}
}

// TEST 5: judge approves additional investigation: only diagnostic actions
// are authorized; the proposed repair never executes.
func TestMandatory5ApprovedInvestigationExecutesNoRepair(t *testing.T) {
	store := NewApprovalStore("")
	coord := &ApprovalCoordinator{Store: store, ProjectID: "proj-1"}
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n"}
	hash := HashAction("manifest_patch", files)
	done := make(chan bool, 1)
	go func() {
		done <- coord.Approver(context.Background(), ApprovalRequest{
			Signature:    "s5",
			Summary:      "investigate the 500 responses",
			Risk:         RiskLow.RiskLabel(),
			Explanation:  "uncertain diagnosis",
			Files:        []string{"docker-compose.yml"},
			Diagnosis:    "health endpoint 500, cause unclear",
			Confidence:   0.5,
			Uncertain:    true,
			Evidence:     []string{"http_probe[probe] api: application_error (code=500)"},
			EvidenceHash: HashEvidence(validEvidence()),
			ActionHash:   hash,
		})
	}()
	a := waitForPending(t, store, "proj-1")
	if !a.Uncertain {
		t.Fatal("approval must expose the uncertainty disclosure")
	}
	// The judge authorizes INVESTIGATION — never the repair.
	if _, err := store.Decide(ApprovalDecision{
		ApprovalID: a.ApprovalID, Decision: DecisionInvestigate, Approved: true,
		ActionHash: hash, ProjectID: "proj-1", DecidedBy: "judge",
	}); err != nil {
		t.Fatalf("investigation approve failed: %v", err)
	}
	if res := <-done; res {
		t.Fatal("investigation approval must NOT execute the repair")
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Decision != DecisionInvestigate || got.Status != ApprovalApproved {
		t.Fatalf("ruling must be recorded as investigation: %+v", got)
	}
	// Even a revalidation attempt must refuse: investigation authorizes
	// diagnostics only, never modification.
	if _, rerr := store.Revalidate(a.ApprovalID, hash, "proj-1"); rerr == nil {
		t.Fatal("investigation ruling must never authorize modification")
	}
}

// TEST 6: uncertain repair proposal exposes uncertainty, evidence, impact.
func TestMandatory6UncertainApprovalDisclosure(t *testing.T) {
	store := NewApprovalStore("")
	coord := &ApprovalCoordinator{Store: store, ProjectID: "proj-1"}
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n"}
	done := make(chan bool, 1)
	go func() {
		done <- coord.Approver(context.Background(), ApprovalRequest{
			Signature:    "s6",
			Summary:      "bump image to clear 500s",
			Risk:         RiskLow.RiskLabel(),
			Explanation:  "low risk",
			Files:        []string{"docker-compose.yml"},
			Diagnosis:    "health endpoint returns 500; cause unclear",
			Confidence:   0.5,
			Uncertain:    true,
			Evidence:     []string{"http_probe[probe] api: application_error (code=500)"},
			EvidenceHash: HashEvidence(validEvidence()),
			ActionHash:   HashAction("manifest_patch", files),
		})
	}()
	a := waitForPending(t, store, "proj-1")
	if !a.Uncertain || a.ConfidenceTier != "MEDIUM_CONFIDENCE" || len(a.Evidence) == 0 || a.ExpectedImpact == "" || a.RollbackPlan == "" {
		t.Fatalf("uncertain approval must disclose uncertainty, evidence, impact and rollback: %+v", a)
	}
	_, _ = store.Decide(ApprovalDecision{ApprovalID: a.ApprovalID, Approved: false, ActionHash: a.ActionHash, ProjectID: "proj-1"})
	if res := <-done; res {
		t.Fatal("rejected uncertain repair must not execute")
	}
}
