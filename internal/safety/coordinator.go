package safety

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ApprovalCoordinator bridges the gate's Approver callback to the
// ApprovalStore: when the policy demands approval, the coordinator creates a
// structured approval request, notifies subscribers, and blocks until a judge
// decides through the backend (web UI, CLI, or tests) or the request expires.
//
// Binding discipline enforced here and in the store:
//   - the action hash is computed by the gate from the exact proposed bytes;
//   - the decision must carry that hash, the project, and the session;
//   - before execution is allowed, Revalidate atomically re-checks status,
//     hash, project, and decision kind — one time only.
type ApprovalCoordinator struct {
	Store     *ApprovalStore
	ProjectID string
	SessionID string
	// Simulated marks every request created through this coordinator as a
	// demonstration event (isolated fixture, never production).
	Simulated bool
	// Emit, when set, receives event notifications when a request is created
	// and resolved (wired to SSE or CLI output).
	Emit func(eventType string, a Approval)
}

// Approver implements the gate's Approver signature.
func (c *ApprovalCoordinator) Approver(ctx context.Context, req ApprovalRequest) bool {
	if c == nil || c.Store == nil {
		return false // no backend configured: refuse (safe default)
	}
	if req.ActionHash == "" {
		return false // an unbound approval is never issued
	}
	a := Approval{
		ProjectID:        c.ProjectID,
		SessionID:        c.SessionID,
		Simulated:        c.Simulated,
		ActionType:       actionTypeFromFiles(req.Files),
		ToolName:         "healer",
		ActionSummary:    firstNonEmpty(req.Summary, req.Explanation),
		TechnicalReason:  req.Diagnosis,
		ConfidenceScore:  req.Confidence,
		ConfidenceTier:   ConfidenceTier(req.Confidence),
		RiskLevel:        req.Risk,
		Evidence:         req.Evidence,
		EvidenceHash:     req.EvidenceHash,
		Uncertain:        req.Uncertain,
		Signature:        req.Signature,
		AffectedFiles:    append([]string{}, req.Files...),
		AffectedServices: affectedServices(req.Files),
		ActionHash:       req.ActionHash,
		RollbackPlan:     "restored generated manifests (healer snapshot) and rolled back the stack on failed verification",
		ExpectedImpact:   fmt.Sprintf("modifies %s; stack rebuilds and health-verifies afterwards", strings.Join(req.Files, ", ")),
	}
	created, err := c.Store.Create(a)
	if err != nil {
		return false
	}
	if c.Emit != nil {
		c.Emit("approval_required", *created)
	}

	// Pause execution until the judge decides or the request expires.
	final, err := c.await(ctx, created.ApprovalID)
	if err != nil {
		return false
	}
	if c.Emit != nil {
		c.Emit(eventForStatus(final.Status), *final)
	}
	// Rejection (or expiry/cancellation) blocks the patch: nothing is modified.
	if final.Status != ApprovalApproved || final.Decision != DecisionRepair {
		return false
	}
	// Revalidate binds execution to the exact approved action hash, one time.
	if _, rerr := c.Store.Revalidate(created.ApprovalID, req.ActionHash, c.ProjectID); rerr != nil {
		return false
	}
	return true
}

// await blocks until the approval reaches a decided status, the request
// expires (a sweeper tick closes it), or the context is cancelled.
func (c *ApprovalCoordinator) await(ctx context.Context, approvalID string) (*Approval, error) {
	ch, cancel := c.Store.Subscribe(8)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if a, ok := c.Store.Get(approvalID); ok && !a.Pending() {
			return a, nil
		}
		select {
		case <-ctx.Done():
			c.Store.CancelExpired()
			return nil, ctx.Err()
		case <-ticker.C:
			c.Store.CancelExpired()
		case ev, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("approval stream closed")
			}
			if ev.Approval.ApprovalID == approvalID && !ev.Approval.Pending() {
				if a, ok := c.Store.Get(approvalID); ok {
					return a, nil
				}
				return &ev.Approval, nil
			}
		}
	}
}

// CompleteAll closes every approval this coordinator moved into EXECUTING,
// recording verification and rollback results for the audit trail.
func (c *ApprovalCoordinator) CompleteAll(success bool, verification, rollback string) {
	if c == nil || c.Store == nil {
		return
	}
	for _, a := range c.Store.List(ApprovalExecuting, c.ProjectID) {
		_, _ = c.Store.Complete(a.ApprovalID, success, verification, rollback)
	}
}

func eventForStatus(s ApprovalStatus) string {
	switch s {
	case ApprovalApproved:
		return "approval_approved"
	case ApprovalRejected:
		return "approval_rejected"
	case ApprovalExpired, ApprovalCancelled:
		return "approval_expired"
	case ApprovalExecuted:
		return "approval_completed"
	case ApprovalFailed:
		return "approval_failed"
	default:
		return "approval_pending"
	}
}

func actionTypeFromFiles(files []string) string {
	if len(files) == 0 {
		return "restart"
	}
	if len(files) == 1 {
		return "manifest_patch"
	}
	return "multi_manifest"
}

func affectedServices(files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		if s := strings.TrimPrefix(f, "Dockerfile."); s != f && s != "" {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
