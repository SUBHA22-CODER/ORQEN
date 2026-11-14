package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

func newTestApprovalAgent(t *testing.T, judge safety.JudgeIdentity) (*Agent, *safety.ApprovalStore) {
	t.Helper()
	store := safety.NewApprovalStore("")
	ag, err := New(Options{Approvals: store, Judge: judge})
	if err != nil {
		t.Fatal(err)
	}
	return ag, store
}

func newPendingApproval(t *testing.T, store *safety.ApprovalStore, summary string) *safety.Approval {
	t.Helper()
	files := map[string]string{"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n"}
	a, err := store.Create(safety.Approval{
		ProjectID:     "proj-1",
		ActionType:    "manifest_patch",
		ActionSummary: summary,
		RiskLevel:     "HIGH_RISK",
		AffectedFiles: []string{"docker-compose.yml"},
		ActionHash:    safety.HashAction("manifest_patch", files),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Chat NL approval: explicit approval produces the judge feedback and records
// the decision; the store shows APPROVED.
func TestChatApprovalExplicitApprove(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	a := newPendingApproval(t, store, "update database hostname in Docker Compose")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "I approve this high-risk repair."})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Approval verified for " + a.ApprovalID,
		"Authorized action: update database hostname",
		"Action hash verified.",
		"Safety checks passed.",
		"Executing approved repair",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("feedback missing %q:\n%s", want, out)
		}
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalApproved || got.Decision != safety.DecisionRepair {
		t.Fatalf("decision must be recorded: %+v", got)
	}
}

// Chat NL rejection: explicitly confirms the modification was NOT executed.
func TestChatApprovalExplicitReject(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	a := newPendingApproval(t, store, "risky change")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "I reject this proposed modification."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "was NOT executed") {
		t.Fatalf("rejection must confirm non-execution:\n%s", out)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalRejected {
		t.Fatalf("expected REJECTED, got %s", got.Status)
	}
}

// Chat NL diagnostics-only: authorizes investigation, never the repair.
func TestChatApprovalDiagnosticsOnly(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	newPendingApproval(t, store, "unclear 500s")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "Approve diagnostics only."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Diagnostics authorized only") || strings.Contains(out, "Executing approved repair") {
		t.Fatalf("diagnostics-only must not authorize repair:\n%s", out)
	}
}

// Chat NL ambiguity: multiple pending approvals without an id → clarification.
func TestChatApprovalAmbiguousClarifies(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	newPendingApproval(t, store, "change one")
	newPendingApproval(t, store, "change two")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "approve it"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "clearer instruction") || !strings.Contains(out, "multiple pending approvals") {
		t.Fatalf("ambiguous approval must clarify:\n%s", out)
	}
	if len(store.List(safety.ApprovalPending, "")) != 2 {
		t.Fatal("ambiguous instruction must not record decisions")
	}
}

// Chat NL hypothetical: no decision is recorded.
func TestChatApprovalHypotheticalDoesNotDecide(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	a := newPendingApproval(t, store, "risky change")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "If I approved this, would the stack recover?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "clearer instruction") {
		t.Fatalf("hypothetical must route to clarification:\n%s", out)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalPending {
		t.Fatalf("hypothetical must not decide, got %s", got.Status)
	}
}

// Chat NL unauthorized: a viewer identity is refused by the verifier.
func TestChatApprovalUnauthorizedRefused(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "intern", Role: safety.RoleViewer})
	a := newPendingApproval(t, store, "risky change")

	_, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "I approve this repair."})
	if err == nil || !strings.Contains(err.Error(), "only judge or reviewer") {
		t.Fatalf("viewer must be refused with an explanation, got: %v", err)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalPending {
		t.Fatal("unauthorized attempts must not change state")
	}
}

// Chat NL show: renders the pending approval details without deciding.
func TestChatApprovalShowDetails(t *testing.T) {
	ag, store := newTestApprovalAgent(t, safety.JudgeIdentity{User: "judge", Role: safety.RoleJudge})
	a := newPendingApproval(t, store, "risky change")

	out, err := ag.handleApprovalIntent(context.Background(), Intent{Raw: "Show me the changes before approval."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, a.ApprovalID) || !strings.Contains(out, "risky change") {
		t.Fatalf("show must render approval details:\n%s", out)
	}
	got, _ := store.Get(a.ApprovalID)
	if got.Status != safety.ApprovalPending {
		t.Fatal("show must not decide")
	}
}
