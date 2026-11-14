package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// storeBackend wraps the fake with a real approval store so API flows can be
// exercised end-to-end over HTTP.
type storeBackend struct {
	fakeBackend
	store *safety.ApprovalStore
}

func (b *storeBackend) ListApprovals(projectID string) ([]safety.Approval, error) {
	var out []safety.Approval
	for _, a := range b.store.List("", projectID) {
		out = append(out, *a)
	}
	return out, nil
}

func (b *storeBackend) GetApproval(id string) (*safety.Approval, error) {
	if a, ok := b.store.Get(id); ok {
		return a, nil
	}
	return nil, os.ErrNotExist
}

func (b *storeBackend) DecideApproval(d safety.ApprovalDecision) (*safety.Approval, error) {
	return b.store.Decide(d)
}

func TestApprovalsAPIListAndDecide(t *testing.T) {
	backend := &storeBackend{store: safety.NewApprovalStore("")}
	ts, _ := newTestServer(t, backend)
	defer ts.Close()

	a, err := backend.store.Create(safety.Approval{
		ProjectID: "proj-1", ActionSummary: "replace compose",
		ActionType: "manifest_patch",
		ActionHash: safety.HashAction("manifest_patch", map[string]string{"docker-compose.yml": "x"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := a.ActionHash

	// LIST: the pending request is visible.
	resp, err := http.Get(ts.URL + "/api/approvals")
	if err != nil {
		t.Fatal(err)
	}
	var list []safety.Approval
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0].ApprovalID != a.ApprovalID || list[0].Status != "PENDING" {
		t.Fatalf("unexpected list: %+v", list)
	}

	// GET by id.
	resp, err = http.Get(ts.URL + "/api/approvals/" + a.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	var got safety.Approval
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.ApprovalID != a.ApprovalID {
		t.Fatalf("get by id failed: %+v", got)
	}

	// APPROVE with a wrong hash: 409, request stays PENDING.
	resp, _ = http.Post(ts.URL+"/api/approvals/"+a.ApprovalID+"/approve", "application/json",
		bytes.NewBufferString(`{"action_hash":"wrong","project_id":"proj-1"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong-hash approve must conflict, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// APPROVE with the exact hash: 200, APPROVED.
	body := `{"action_hash":"` + hash + `","project_id":"proj-1","decision":"repair"}`
	resp, _ = http.Post(ts.URL+"/api/approvals/"+a.ApprovalID+"/approve", "application/json",
		bytes.NewBufferString(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve must succeed, got %d", resp.StatusCode)
	}
	var decided safety.Approval
	_ = json.NewDecoder(resp.Body).Decode(&decided)
	resp.Body.Close()
	if decided.Status != safety.ApprovalApproved || decided.Decision != safety.DecisionRepair {
		t.Fatalf("unexpected decision result: %+v", decided)
	}

	// REPLAY: deciding again fails with 409.
	resp, _ = http.Post(ts.URL+"/api/approvals/"+a.ApprovalID+"/approve", "application/json",
		bytes.NewBufferString(body))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("replay must conflict, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestApprovalsAPICSRFAndDemo(t *testing.T) {
	backend := &storeBackend{store: safety.NewApprovalStore("")}
	ts, _ := newTestServer(t, backend)
	defer ts.Close()

	// Cross-origin state-changing request is rejected (CSRF guard).
	req, _ := http.NewRequest("POST", ts.URL+"/api/approvals/demo", bytes.NewBufferString(`{"event":"LOW_RISK_FIX"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST must be forbidden, got %d", resp.StatusCode)
	}

	// Same-origin (no Origin header for server-to-server calls) passes
	// through to the backend stub.
	resp, err = http.Post(ts.URL+"/api/approvals/demo", "application/json",
		bytes.NewBufferString(`{"event":"LOW_RISK_FIX"}`))
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&report)
	resp.Body.Close()
	if report["simulated"] != true {
		t.Fatalf("demo response must be labeled simulated: %v", report)
	}
}
