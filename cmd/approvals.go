package cmd

import (
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/config"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// judgeApprovals activates the judge HITL approval backend for healers
// created while the web control plane is running (set by `orqen serve`).
var judgeApprovals bool

// approvalStoreOnce guards the process-wide approval store instance. A single
// instance is mandatory: the web backend, the healer's approval coordinator,
// and the expiry sweeper must share one in-memory state, otherwise decisions
// made in one component would never unblock another.
var (
	approvalStoreMu     sync.Mutex
	approvalStoreShared *safety.ApprovalStore
)

// sharedApprovalStore returns the process-wide approval store persisted at
// ~/.orqen/approvals.json (the same location the web backend uses). Tests may
// swap in a memory store via approvalStoreOverride so CLI runs never touch
// the real file.
var approvalStoreOverride *safety.ApprovalStore

func sharedApprovalStore() *safety.ApprovalStore {
	if approvalStoreOverride != nil {
		return approvalStoreOverride
	}
	approvalStoreMu.Lock()
	defer approvalStoreMu.Unlock()
	if approvalStoreShared == nil {
		path := ""
		if h, err := config.Home(); err == nil && h != "" {
			path = filepath.Join(h, "approvals.json")
		}
		approvalStoreShared = safety.NewApprovalStore(path)
	}
	return approvalStoreShared
}

// judgeCoordinator returns the approval coordinator for a project, bound to
// the shared store so judge decisions made in the web UI unblock healers.
func judgeCoordinator(projectID string) *safety.ApprovalCoordinator {
	return &safety.ApprovalCoordinator{
		Store:     sharedApprovalStore(),
		ProjectID: projectID,
	}
}

// expirySweeper cancels expired PENDING approvals periodically until stop is
// closed. serve starts it so paused pipelines unblock deterministically.
func expirySweeper(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			sharedApprovalStore().CancelExpired()
		}
	}
}

// approvalsAsUser overrides the authenticated identity for CLI approval
// commands (--as). Empty means the current OS user.
var approvalsAsUser string

// currentJudgeIdentity resolves the authenticated judge identity: the OS
// user (or --as override) checked against the judges roster, which is
// bootstrapped on first run with the OS user as judge.
func currentJudgeIdentity() safety.JudgeIdentity {
	username := approvalsAsUser
	if username == "" {
		if u, err := user.Current(); err == nil {
			username = u.Username
		} else {
			username = "local"
		}
	}
	home, _ := config.Home()
	return safety.ResolveJudgeIdentity(home, username)
}
