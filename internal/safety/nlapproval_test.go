package safety

import (
	"strings"
	"testing"
	"time"
)

// --- natural-language recognition (spec §1) ----------------------------------

func TestRecognizeExplicitApproval(t *testing.T) {
	na := RecognizeApproval("I approve this high-risk repair.")
	if na.Kind != NLApprove || !na.Explicit || na.Scope != NLScopeRepair {
		t.Fatalf("explicit approval misread: %+v", na)
	}
	if na.Hypothetical || na.Quoted || len(na.Ambiguities) != 0 {
		t.Fatalf("clean approval must carry no caveats: %+v", na)
	}
}

func TestRecognizeApprovalWithID(t *testing.T) {
	na := RecognizeApproval("Proceed with approval ORQEN-101.")
	if na.Kind != NLApprove || na.ApprovalID != "ORQEN-101" || !na.Explicit {
		t.Fatalf("approval with id misread: %+v", na)
	}
}

func TestRecognizeExplicitRejection(t *testing.T) {
	na := RecognizeApproval("I reject this proposed modification.")
	if na.Kind != NLReject || !na.Explicit {
		t.Fatalf("explicit rejection misread: %+v", na)
	}
	// "Do not approve" phrasing is also an explicit rejection, not a question.
	na2 := RecognizeApproval("Do not approve this repair.")
	if na2.Kind != NLReject || !na2.Explicit || na2.Hypothetical {
		t.Fatalf("negation rejection misread: %+v", na2)
	}
}

func TestRecognizeShowChanges(t *testing.T) {
	na := RecognizeApproval("Show me the changes before approval.")
	if na.Kind != NLShow {
		t.Fatalf("show request misread: %+v", na)
	}
	if na.Explicit {
		t.Fatal("showing changes is never an authorization")
	}
}

func TestRecognizeInvestigateFurther(t *testing.T) {
	na := RecognizeApproval("Investigate further; do not repair yet.")
	if na.Kind != NLInvestigate || na.Scope != NLScopeDiagnostics {
		t.Fatalf("investigate instruction misread: %+v", na)
	}
}

func TestRecognizeDiagnosticsOnly(t *testing.T) {
	na := RecognizeApproval("Approve diagnostics only.")
	if na.Kind != NLInvestigate || na.Scope != NLScopeDiagnostics {
		t.Fatalf("diagnostics-only misread: %+v", na)
	}
}

func TestRecognizeAmbiguous(t *testing.T) {
	na := RecognizeApproval("please handle it")
	if na.Kind != NLUnclear || len(na.Ambiguities) == 0 {
		t.Fatalf("ambiguous instruction must be unclear with reasons: %+v", na)
	}
	// A bare "approve" with no object is ambiguous by design: it names no
	// action and no target, so it must route to clarification.
	na2 := RecognizeApproval("approve")
	if na2.Kind != NLUnclear || len(na2.Ambiguities) == 0 {
		t.Fatalf("bare approve must be unclear with reasons: %+v", na2)
	}
	// "approve it" with nothing pending: no target to bind.
	na3 := RecognizeApproval("approve it")
	if na3.Kind != NLApprove {
		t.Fatalf("approve it should classify as approve: %+v", na3)
	}
	bound := BindIntent(na3, nil)
	joined := strings.Join(bound.Ambiguities, "; ")
	if bound.Kind != NLUnclear || !strings.Contains(joined, "no pending approval") {
		t.Fatalf("approve with nothing pending must need clarification: %+v", bound)
	}
}

func TestRecognizeQuestionAndHypotheticalAndQuoted(t *testing.T) {
	// Questions are marked hypothetical and never explicit: they cannot
	// authorize, no matter which approval words they contain.
	q := RecognizeApproval("Can I approve ORQEN-101?")
	if !q.Hypothetical || q.Explicit {
		t.Fatalf("question must be flagged hypothetical and inexplicit: %+v", q)
	}
	h := RecognizeApproval("If I approved ORQEN-101, would that fix it?")
	if !h.Hypothetical || h.Explicit || h.Kind == NLUnclear {
		t.Fatalf("hypothetical misread: %+v", h)
	}
	quoted := RecognizeApproval(`The manager said 'approve the repair' this morning.`)
	if !quoted.Quoted || quoted.Explicit {
		t.Fatalf("quoted approval language must not authorize: %+v", quoted)
	}
}

// --- mandatory scenario fixtures ----------------------------------------------

func newNLApproval(t *testing.T, store *ApprovalStore, summary string) *Approval {
	t.Helper()
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n"}
	a, err := store.Create(Approval{
		ProjectID:     "proj-1",
		SessionID:     "sess-1",
		ActionType:    "manifest_patch",
		ActionSummary: summary,
		RiskLevel:     "HIGH_RISK",
		AffectedFiles: []string{"docker-compose.yml"},
		ActionHash:    HashAction("manifest_patch", files),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var judge = JudgeIdentity{User: "judge-1", Role: RoleJudge}
var viewer = JudgeIdentity{User: "intern", Role: RoleViewer}

// MANDATORY 1: explicit approval authorizes exactly the recorded action.
func TestNL1ExplicitApprovalAuthorizesExactAction(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "update database hostname in Docker Compose")
	na := BindIntent(RecognizeApproval("I approve this high-risk repair."), store.List(ApprovalPending, ""))
	if na.ApprovalID != a.ApprovalID {
		t.Fatalf("binding failed: %+v", na)
	}
	decided, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: na.ApprovalID, RequiredScope: DecisionRepair,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != ApprovalExecuting || complete == nil {
		t.Fatalf("approval must be locked for execution: %+v", decided)
	}
	// Only THIS approval can execute: one-time, and nothing else is armed.
	if _, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("second revalidation of the same approval must be refused (one-time)")
	}
	b := newNLApproval(t, store, "another action")
	if _, err := store.Revalidate(b.ApprovalID, b.ActionHash, b.ProjectID); err == nil {
		t.Fatal("an unapproved approval must not execute")
	}
	if err := complete(true, "health verified", "snapshot retained"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalExecuted {
		t.Fatalf("completed approval must be EXECUTED, got %s", got.Status)
	}
}

// MANDATORY 2: explicit rejection records the refusal; nothing executes.
func TestNL2ExplicitRejection(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	na := BindIntent(RecognizeApproval("I reject this proposed modification."), store.List(ApprovalPending, ""))
	if _, err := VerifyAuthorization(store, VerifyRequest{Identity: judge, ApprovalID: na.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	decided, err := store.Decide(ApprovalDecision{
		ApprovalID: a.ApprovalID, Approved: false,
		ActionHash: a.ActionHash, ProjectID: a.ProjectID, DecidedBy: judge.User,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != ApprovalRejected {
		t.Fatalf("expected REJECTED, got %s", decided.Status)
	}
	// The rejected operation can never execute.
	if _, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("rejected approval must not execute")
	}
}

// MANDATORY 3: ambiguous approval triggers clarification, never a decision.
func TestNL3AmbiguousRequiresClarification(t *testing.T) {
	store := NewApprovalStore("")
	newNLApproval(t, store, "change one")
	newNLApproval(t, store, "change two")
	na := RecognizeApproval("approve it")
	na = BindIntent(na, store.List(ApprovalPending, ""))
	if na.Kind != NLUnclear || len(na.Ambiguities) == 0 {
		t.Fatalf("multiple pending approvals without an id must clarify: %+v", na)
	}
	if len(store.List(ApprovalPending, "")) != 2 {
		t.Fatal("ambiguous instruction must not record any decision")
	}
}

// MANDATORY 4: hypothetical approval language never decides.
func TestNL4HypotheticalApproval(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	na := RecognizeApproval("If I approved this repair, would the stack recover?")
	if !na.Hypothetical || na.Explicit {
		t.Fatalf("hypothetical must not be explicit: %+v", na)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalPending {
		t.Fatalf("hypothetical language must not decide, got %s", got.Status)
	}
}

// MANDATORY 5: unauthorized user attempting approval.
func TestNL5UnauthorizedUserRefused(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	if _, err := VerifyAuthorization(store, VerifyRequest{Identity: viewer, ApprovalID: a.ApprovalID}); err == nil {
		t.Fatal("viewer must not be able to decide approvals")
	}
	if _, err := VerifyAuthorization(store, VerifyRequest{Identity: JudgeIdentity{}, ApprovalID: a.ApprovalID}); err == nil {
		t.Fatal("missing identity must be refused")
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalPending {
		t.Fatal("unauthorized attempts must not change state")
	}
}

// MANDATORY 6: expired approval cannot be authorized.
func TestNL6ExpiredApprovalRefused(t *testing.T) {
	store := NewApprovalStore("")
	files := map[string]string{"docker-compose.yml": "x"}
	a, err := store.Create(Approval{
		ProjectID: "proj-1", ActionSummary: "late", ActionType: "manifest_patch",
		ActionHash: HashAction("manifest_patch", files),
		ExpiresAt:  time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAuthorization(store, VerifyRequest{Identity: judge, ApprovalID: a.ApprovalID}); err == nil {
		t.Fatal("expired approval must fail verification")
	}
}

// MANDATORY 7: replayed approval is refused.
func TestNL7ReplayedApprovalRefused(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	if _, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	}); err != nil || complete == nil {
		t.Fatalf("first execution must succeed: %v", err)
	}
	if _, err := VerifyAuthorization(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	}); err == nil {
		t.Fatal("replayed authorization must be refused")
	}
}

// MANDATORY 8: modified action after approval: hash mismatch blocks.
func TestNL8ModifiedActionBlocked(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	if _, err := VerifyAuthorization(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID,
		ActionHash: "deadbeef",
	}); err == nil {
		t.Fatal("hash mismatch must fail verification")
	}
	if _, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	}); err != nil || complete == nil {
		t.Fatalf("correct-hash execution must proceed: %v", err)
	}
}

// MANDATORY 9: approval for the wrong project is refused.
func TestNL9WrongProjectRefused(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	if _, err := VerifyAuthorization(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, ProjectID: "other-project",
	}); err == nil {
		t.Fatal("project binding mismatch must be refused")
	}
	if _, err := VerifyAuthorization(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, ProjectID: "proj-1",
	}); err != nil {
		t.Fatalf("correct project must verify: %v", err)
	}
}

// MANDATORY 10: diagnostics-only approval never executes a repair.
func TestNL10DiagnosticsOnlyNeverExecutes(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "investigate the 500s")
	decided, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionInvestigate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Decision != DecisionInvestigate {
		t.Fatalf("ruling must be investigation, got %s", decided.Decision)
	}
	if complete != nil {
		t.Fatal("diagnostics approval must not carry an execution hook")
	}
	if _, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("diagnostics ruling must never authorize modification")
	}
}

// MANDATORY 11: execution failure records the outcome and rollback.
func TestNL11ExecutionFailureRecorded(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "risky change")
	_, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	})
	if err != nil || complete == nil {
		t.Fatal(err)
	}
	if err := complete(false, "health verification failed after apply", "previous configuration restored from snapshot"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalFailed || got.RollbackResult == "" {
		t.Fatalf("failure must record verification and rollback: %+v", got)
	}
	if _, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("failed approval must not re-execute")
	}
}

// MANDATORY 12: attempt to expand approved scope is refused.
func TestNL12ScopeExpansionRefused(t *testing.T) {
	store := NewApprovalStore("")
	a := newNLApproval(t, store, "update database hostname in Docker Compose")
	if _, complete, err := ExecuteApproved(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	}); err != nil || complete == nil {
		t.Fatal(err)
	}
	// Expanding the authorized action: a different action hash under the same
	// approval id is refused.
	expanded := map[string]string{
		"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n",
		"Dockerfile.api":     "FROM python:3.12-slim\n",
	}
	if _, err := store.Revalidate(a.ApprovalID, HashAction("multi_manifest", expanded), a.ProjectID); err == nil {
		t.Fatal("expanded action hash must be refused")
	}
	// And one approval can never be piggybacked onto another's authorization.
	b := newNLApproval(t, store, "unrelated action")
	if _, err := store.Revalidate(b.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("one approval must never authorize another action")
	}
}

// MANDATORY 13: critical destructive operation blocked by policy.
func TestNL13CriticalBlockedByPolicy(t *testing.T) {
	store := NewApprovalStore("")
	files := map[string]string{"docker-compose.yml": "services:\n  db:\n# docker compose down -v\n"}
	a, err := store.Create(Approval{
		ProjectID: "proj-1", ActionType: "manifest_patch",
		ActionSummary: "wipe stack volumes",
		ProposedDiff:  files["docker-compose.yml"],
		RiskLevel:     "CRITICAL_RISK",
		ActionHash:    HashAction("manifest_patch", files),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Even an authorized judge cannot approve it: policy revalidation refuses.
	if _, err := VerifyAuthorization(store, VerifyRequest{
		Identity: judge, ApprovalID: a.ApprovalID, RequiredScope: DecisionRepair,
	}); err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("prohibited operation must be refused by policy, got: %v", err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != ApprovalPending {
		t.Fatalf("prohibited approval must remain undecided, got %s", got.Status)
	}
	if _, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID); err == nil {
		t.Fatal("prohibited approval must never execute")
	}
}
