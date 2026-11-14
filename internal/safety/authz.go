package safety

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Judge authorization: identity and permission checks for approval decisions.
// This is deliberately code, not model judgment — an LLM can phrase a request
// but never grants authorization.

// JudgeRole is the permission level of an operator.
type JudgeRole string

const (
	RoleJudge    JudgeRole = "judge"    // may approve and reject
	RoleReviewer JudgeRole = "reviewer" // may approve and reject
	RoleViewer   JudgeRole = "viewer"   // read-only: never authorizes
)

// MayDecide reports whether the role can approve or reject approvals.
func (r JudgeRole) MayDecide() bool {
	return r == RoleJudge || r == RoleReviewer
}

// JudgeIdentity is an authenticated operator.
type JudgeIdentity struct {
	User string    `json:"user"`
	Role JudgeRole `json:"role"`
}

// judgesConfig is the persisted judge roster at ~/.orqen/judges.json.
type judgesConfig struct {
	Judges []JudgeIdentity `json:"judges"`
}

// ResolveJudgeIdentity returns the authenticated identity for user. When the
// roster file does not exist it is bootstrapped with the OS user as judge so
// a fresh install works out of the box while remaining verifiable. A user
// missing from an existing roster resolves to a viewer (no permissions).
func ResolveJudgeIdentity(home, user string) JudgeIdentity {
	if user == "" {
		return JudgeIdentity{User: "", Role: RoleViewer}
	}
	path := filepath.Join(home, "judges.json")
	role := RoleViewer
	if data, err := os.ReadFile(path); err == nil {
		var cfg judgesConfig
		if json.Unmarshal(data, &cfg) == nil {
			for _, j := range cfg.Judges {
				if j.User == user {
					role = j.Role
					break
				}
			}
			return JudgeIdentity{User: user, Role: role}
		}
	}
	// Bootstrap: first run grants the OS user the judge role.
	cfg := judgesConfig{Judges: []JudgeIdentity{{User: user, Role: RoleJudge}}}
	if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(path, append(data, '\n'), 0o600)
	}
	return JudgeIdentity{User: user, Role: RoleJudge}
}

// VerifyRequest is the preflight for one approval decision. Every field is
// checked in code; nothing here consults an LLM.
type VerifyRequest struct {
	Identity JudgeIdentity
	// ApprovalID is the exact pending request being ruled on.
	ApprovalID string
	// ActionHash, when non-empty, must match the stored hash exactly.
	ActionHash string
	// ProjectID, when non-empty, must match the stored project.
	ProjectID string
	// SessionID, when non-empty, must match the stored session.
	SessionID string
	// RequiredScope is the decision kind the caller intends to grant
	// (repair or investigation). Diagnostics grants never authorize repairs.
	RequiredScope DecisionKind
}

// VerifyAuthorization runs the full preflight checklist against the store:
//
//  1. authenticated identity with judge/reviewer permissions
//  2. the exact pending approval exists
//  3. still PENDING (no conflicting decision recorded)
//  4. not expired
//  5. project and session binding
//  6. immutable action-hash binding
//  7. safety-policy revalidation (prohibited operations refuse even a judge)
//
// It returns the resolved approval for the subsequent atomic Decide call.
func VerifyAuthorization(store *ApprovalStore, v VerifyRequest) (*Approval, error) {
	// 1. Identity and permissions.
	if v.Identity.User == "" {
		return nil, fmt.Errorf("authorization refused: no authenticated identity")
	}
	if !v.Identity.Role.MayDecide() {
		return nil, fmt.Errorf("authorization refused: %q has role %q; only judge or reviewer may decide approvals", v.Identity.User, v.Identity.Role)
	}

	// 2. Exact pending approval.
	a, ok := store.Get(v.ApprovalID)
	if !ok {
		return nil, fmt.Errorf("approval %s not found", v.ApprovalID)
	}

	// 3. Still pending: any recorded decision is a conflicting decision.
	if !a.Pending() {
		return nil, fmt.Errorf("approval %s is %s; a decision has already been recorded", a.ApprovalID, a.Status)
	}

	// 4. Expiration.
	if a.ExpiredAt(time.Now().UTC()) {
		return nil, fmt.Errorf("approval %s expired at %s", a.ApprovalID, a.ExpiresAt.Format(time.RFC3339))
	}

	// 5. Project and session binding.
	if v.ProjectID != "" && v.ProjectID != a.ProjectID {
		return nil, fmt.Errorf("approval %s belongs to project %q, not %q", a.ApprovalID, a.ProjectID, v.ProjectID)
	}
	if a.SessionID != "" && v.SessionID != "" && v.SessionID != a.SessionID {
		return nil, fmt.Errorf("approval %s belongs to session %q, not %q", a.ApprovalID, a.SessionID, v.SessionID)
	}

	// 6. Immutable action-hash binding.
	if v.ActionHash != "" && v.ActionHash != a.ActionHash {
		return nil, fmt.Errorf("approval %s action-hash mismatch: authorization is bound to the exact proposed action", a.ApprovalID)
	}

	// 7. Policy revalidation: prohibited content refuses even an authorized
	// judge. The stored proposed diff and summary are re-checked against the
	// current policy at decision time.
	joined := a.ProposedDiff + "\n" + a.ActionSummary
	if markers := Prohibited(joined); len(markers) > 0 {
		return nil, fmt.Errorf("approval %s refused by policy: prohibited operation (%s); no approval can authorize it", a.ApprovalID, strings.Join(markers, "; "))
	}
	return a, nil
}

// ExecuteApproved runs the post-authorization pipeline for an approval:
// record the decision atomically, lock the request against replay, verify
// the approved action hash, and hand back a completion hook for the caller's
// execution stage. For investigation rulings nothing may execute: the return
// carries the ruling and a nil completion hook, and the caller reports that
// only diagnostics are authorized.
func ExecuteApproved(store *ApprovalStore, v VerifyRequest) (*Approval, func(success bool, verification, rollback string) error, error) {
	a, err := VerifyAuthorization(store, v)
	if err != nil {
		return nil, nil, err
	}
	// Record the decision (atomic compare-and-swap) and lock against replay.
	decided, err := store.Decide(ApprovalDecision{
		ApprovalID: a.ApprovalID,
		Decision:   v.RequiredScope,
		Approved:   true,
		ActionHash: a.ActionHash,
		ProjectID:  a.ProjectID,
		SessionID:  a.SessionID,
		DecidedBy:  v.Identity.User,
	})
	if err != nil {
		return nil, nil, err
	}
	if decided.Decision != DecisionRepair {
		// Diagnostics-only: no modification is permitted, nothing executes.
		return decided, nil, nil
	}
	// Replay lock + hash verification; transitions to EXECUTING exactly once.
	locked, err := store.Revalidate(a.ApprovalID, a.ActionHash, a.ProjectID)
	if err != nil {
		return decided, nil, err
	}
	complete := func(success bool, verification, rollback string) error {
		_, cerr := store.Complete(locked.ApprovalID, success, verification, rollback)
		return cerr
	}
	return locked, complete, nil
}
