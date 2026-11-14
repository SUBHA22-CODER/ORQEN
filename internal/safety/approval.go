package safety

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/safefs"
)

// ApprovalStatus is the lifecycle state of one approval request.
type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "PENDING"
	ApprovalApproved  ApprovalStatus = "APPROVED"
	ApprovalRejected  ApprovalStatus = "REJECTED"
	ApprovalExpired   ApprovalStatus = "EXPIRED"
	ApprovalExecuting ApprovalStatus = "EXECUTING"
	ApprovalExecuted  ApprovalStatus = "EXECUTED"
	ApprovalFailed    ApprovalStatus = "FAILED"
	ApprovalCancelled ApprovalStatus = "CANCELLED"
)

// Terminal reports whether the status can no longer change.
func (s ApprovalStatus) Terminal() bool {
	switch s {
	case ApprovalExecuted, ApprovalFailed, ApprovalRejected, ApprovalCancelled, ApprovalExpired:
		return true
	}
	return false
}

// DecisionKind distinguishes what a judge actually authorized: a proposed
// repair, or additional autonomous investigation. The two must never be
// confused — approving investigation does NOT authorize any modification.
type DecisionKind string

const (
	DecisionRepair      DecisionKind = "repair"
	DecisionInvestigate DecisionKind = "investigation"
)

// Approval is one structured, immutable-bound approval request. Its identity
// is the pair (approval_id, action_hash): an approval authorizes exactly one
// proposed action, never a class of actions.
type Approval struct {
	// Identity and scope.
	ApprovalID string    `json:"approval_id"`
	ProjectID  string    `json:"project_id"`
	SessionID  string    `json:"session_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`

	// What is being proposed.
	ActionType       string   `json:"action_type"` // manifest_patch | multi_manifest | restart | infra_change | investigation
	ToolName         string   `json:"tool_name,omitempty"`
	ActionSummary    string   `json:"action_summary"`
	TechnicalReason  string   `json:"technical_reason,omitempty"`
	AffectedFiles    []string `json:"affected_files,omitempty"`
	AffectedServices []string `json:"affected_services,omitempty"`
	ProposedDiff     string   `json:"proposed_diff,omitempty"`
	ExpectedImpact   string   `json:"expected_impact,omitempty"`
	RollbackPlan     string   `json:"rollback_plan,omitempty"`

	// Independent classification (never LLM-reported).
	ConfidenceScore float64  `json:"confidence_score"`
	ConfidenceTier  string   `json:"confidence_tier"`
	RiskLevel       string   `json:"risk_level"`
	Evidence        []string `json:"evidence,omitempty"`
	EvidenceHash    string   `json:"evidence_hash,omitempty"`
	Uncertain       bool     `json:"uncertain,omitempty"` // uncertain-but-actionable disclosure
	Signature       string   `json:"signature,omitempty"` // failure fingerprint
	Simulated       bool     `json:"simulated,omitempty"` // demo/simulated event marker

	// Binding: the exact immutable action this approval authorizes.
	ActionHash string `json:"action_hash"`

	// Status and decision.
	Status          ApprovalStatus `json:"status"`
	Decision        DecisionKind   `json:"decision,omitempty"`
	DecidedBy       string         `json:"decided_by,omitempty"`
	DecidedAt       time.Time      `json:"decided_at,omitempty"`
	RejectionReason string         `json:"rejection_reason,omitempty"`

	// Execution outcome.
	ExecutedAt         time.Time `json:"executed_at,omitempty"`
	VerificationResult string    `json:"verification_result,omitempty"`
	RollbackResult     string    `json:"rollback_result,omitempty"`
}

// Pending reports whether the approval can still be decided.
func (a *Approval) Pending() bool { return a.Status == ApprovalPending }

// ExpiredAt reports whether the approval is past its expiry (PENDING only:
// decided requests keep their original expiry for audit purposes).
func (a *Approval) ExpiredAt(now time.Time) bool {
	return a.Pending() && !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt)
}

// ApprovalDecision is the judge's ruling submitted through the backend.
type ApprovalDecision struct {
	// ApprovalID identifies the request being ruled on.
	ApprovalID string `json:"approval_id"`
	// Decision is what was authorized: repair or investigation.
	Decision DecisionKind `json:"decision"`
	// Approved is false to reject.
	Approved bool `json:"approved"`
	// ActionHash must match the stored approval's hash exactly.
	ActionHash string `json:"action_hash"`
	// ProjectID and SessionID scope the authorization.
	ProjectID string `json:"project_id"`
	SessionID string `json:"session_id,omitempty"`
	// DecidedBy records who ruled (e.g. "judge-ui", "cli", "test").
	DecidedBy string `json:"decided_by,omitempty"`
	// Reason is recorded for rejections and audit.
	Reason string `json:"reason,omitempty"`
}

// DefaultApprovalTTL is how long a PENDING approval stays decidable.
const DefaultApprovalTTL = 15 * time.Minute

// ApprovalEvent is broadcast to subscribers (SSE streams) on every change.
type ApprovalEvent struct {
	Type     string    `json:"type"` // approval_required | approval_pending | approval_approved | approval_rejected | approval_expired | approval_executing | approval_completed | approval_failed
	Approval Approval  `json:"approval"`
	At       time.Time `json:"at"`
}

// ApprovalStore is the approval backend: create, list, get, decide, cancel
// expired, and history. Decisions are atomic compare-and-swaps: exactly one
// caller can transition a request out of PENDING, which makes concurrent
// approval attempts deterministic. State persists as 0600 JSON via safefs.
type ApprovalStore struct {
	mu        sync.Mutex
	path      string
	approvals []*Approval
	subs      map[chan ApprovalEvent]struct{}
	now       func() time.Time
	maxRecs   int
}

// NewApprovalStore loads (or creates) a store persisted at path. Empty path
// means memory-only (used by tests and the demo harness).
func NewApprovalStore(path string) *ApprovalStore {
	s := &ApprovalStore{
		path:    path,
		subs:    map[chan ApprovalEvent]struct{}{},
		now:     func() time.Time { return time.Now().UTC() },
		maxRecs: 200,
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var wrapper struct {
				Approvals []*Approval `json:"approvals"`
			}
			if json.Unmarshal(data, &wrapper) == nil {
				s.approvals = wrapper.Approvals
			}
		}
	}
	return s
}

// Subscribe returns a channel receiving approval events until cancel is
// called. The channel is buffered; slow consumers drop events rather than
// block the repair pipeline.
func (s *ApprovalStore) Subscribe(buffer int) (<-chan ApprovalEvent, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	ch := make(chan ApprovalEvent, buffer)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, cancel
}

func (s *ApprovalStore) broadcast(ev ApprovalEvent) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // drop rather than block the repair pipeline
		}
	}
}

// Create registers a new PENDING approval and returns it. ActionHash is
// mandatory: an approval without an immutable action binding is refused.
func (s *ApprovalStore) Create(a Approval) (*Approval, error) {
	if strings.TrimSpace(a.ActionHash) == "" {
		return nil, fmt.Errorf("approval: action_hash is required")
	}
	if strings.TrimSpace(a.ApprovalID) == "" {
		a.ApprovalID = s.nextID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = a.CreatedAt.Add(DefaultApprovalTTL)
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	if a.Status != ApprovalPending {
		return nil, fmt.Errorf("approval: created requests must start PENDING, got %s", a.Status)
	}
	if a.ConfidenceTier == "" {
		a.ConfidenceTier = ConfidenceTier(a.ConfidenceScore)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.approvals {
		if ex.ApprovalID == a.ApprovalID {
			return nil, fmt.Errorf("approval: duplicate id %s", a.ApprovalID)
		}
	}
	s.approvals = append(s.approvals, &a)
	if len(s.approvals) > s.maxRecs {
		s.approvals = s.approvals[len(s.approvals)-s.maxRecs:]
	}
	_ = s.save()
	s.broadcast(ApprovalEvent{Type: "approval_required", Approval: a, At: s.now()})
	s.broadcast(ApprovalEvent{Type: "approval_pending", Approval: a, At: s.now()})
	return &a, nil
}

// nextID mints a random hex id.
func (s *ApprovalStore) nextID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("apr-%d", s.now().UnixNano())
	}
	return "apr-" + hex.EncodeToString(b)
}

// List returns copies of the matching approvals, newest first for UI
// consumption. Copies (not live pointers) are returned so callers can read
// records concurrently with store transitions without data races.
func (s *ApprovalStore) List(status ApprovalStatus, projectID string) []*Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Approval
	for _, a := range s.approvals {
		if status != "" && a.Status != status {
			continue
		}
		if projectID != "" && a.ProjectID != projectID {
			continue
		}
		c := *a
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get returns a copy of one approval by id. The copy is safe to read while
// other goroutines transition the live record.
func (s *ApprovalStore) Get(approvalID string) (*Approval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.approvals {
		if a.ApprovalID == approvalID {
			c := *a
			return &c, true
		}
	}
	return nil, false
}

// Decide applies the judge's ruling with full revalidation:
//   - the approval must exist, be PENDING, and not expired;
//   - the action hash, project id, and session id must match exactly;
//   - approvals must be one-time: exactly one caller wins the
//     compare-and-swap, concurrent decisions deterministically fail.
func (s *ApprovalStore) Decide(d ApprovalDecision) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.findLocked(d.ApprovalID)
	if a == nil {
		return nil, fmt.Errorf("approval %s not found", d.ApprovalID)
	}
	if a.Pending() && a.ExpiredAt(s.now()) {
		a.Status = ApprovalExpired
		_ = s.save()
		s.broadcast(ApprovalEvent{Type: "approval_expired", Approval: *a, At: s.now()})
		return nil, fmt.Errorf("approval %s expired at %s", a.ApprovalID, a.ExpiresAt.Format(time.RFC3339))
	}
	if !a.Pending() {
		return nil, fmt.Errorf("approval %s is %s and can no longer be decided", a.ApprovalID, a.Status)
	}
	if a.ActionHash != d.ActionHash {
		return nil, fmt.Errorf("approval %s action-hash mismatch: authorization is bound to the exact proposed action", a.ApprovalID)
	}
	if d.ProjectID != "" && d.ProjectID != a.ProjectID {
		return nil, fmt.Errorf("approval %s belongs to project %q, not %q", a.ApprovalID, a.ProjectID, d.ProjectID)
	}
	if a.SessionID != "" && d.SessionID != "" && d.SessionID != a.SessionID {
		return nil, fmt.Errorf("approval %s belongs to session %q, not %q", a.ApprovalID, a.SessionID, d.SessionID)
	}
	if !d.Approved {
		a.Status = ApprovalRejected
		a.Decision = d.Decision
		a.DecidedBy = d.DecidedBy
		a.DecidedAt = s.now()
		a.RejectionReason = d.Reason
		_ = s.save()
		s.broadcast(ApprovalEvent{Type: "approval_rejected", Approval: *a, At: s.now()})
		return a, nil
	}
	if d.Decision != DecisionRepair && d.Decision != DecisionInvestigate {
		return nil, fmt.Errorf("approval decision kind must be repair or investigation")
	}
	a.Status = ApprovalApproved
	a.Decision = d.Decision
	a.DecidedBy = d.DecidedBy
	a.DecidedAt = s.now()
	_ = s.save()
	s.broadcast(ApprovalEvent{Type: "approval_approved", Approval: *a, At: s.now()})
	return a, nil
}

func (s *ApprovalStore) findLocked(id string) *Approval {
	for _, a := range s.approvals {
		if a.ApprovalID == id {
			return a
		}
	}
	return nil
}

// Revalidate is the pre-execution gate: it verifies the approval is APPROVED
// with the exact action hash, atomically transitions it to EXECUTING (one
// time), and returns the ruling. Any mismatch blocks execution.
func (s *ApprovalStore) Revalidate(approvalID, actionHash, projectID string) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.findLocked(approvalID)
	if a == nil {
		return nil, fmt.Errorf("approval %s not found", approvalID)
	}
	if a.Status == ApprovalPending && a.ExpiredAt(s.now()) {
		a.Status = ApprovalExpired
		_ = s.save()
		s.broadcast(ApprovalEvent{Type: "approval_expired", Approval: *a, At: s.now()})
	}
	switch a.Status {
	case ApprovalApproved:
	case ApprovalExecuting:
		return nil, fmt.Errorf("approval %s is already executing; approvals are one-time", approvalID)
	case ApprovalExecuted:
		return nil, fmt.Errorf("approval %s was already executed; approvals are one-time", approvalID)
	default:
		return nil, fmt.Errorf("approval %s is %s; execution requires APPROVED", approvalID, a.Status)
	}
	if a.ActionHash != actionHash {
		return nil, fmt.Errorf("approval %s action-hash mismatch at revalidation: the proposed action changed after approval", approvalID)
	}
	if projectID != "" && a.ProjectID != projectID {
		return nil, fmt.Errorf("approval %s belongs to project %q, not %q", approvalID, a.ProjectID, projectID)
	}
	if a.Decision != DecisionRepair {
		return nil, fmt.Errorf("approval %s authorized %s only: no modification is permitted", approvalID, a.Decision)
	}
	a.Status = ApprovalExecuting
	a.ExecutedAt = s.now()
	_ = s.save()
	s.broadcast(ApprovalEvent{Type: "approval_executing", Approval: *a, At: s.now()})
	return a, nil
}

// Complete records the final execution outcome and closes the lifecycle.
func (s *ApprovalStore) Complete(approvalID string, success bool, verification, rollback string) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.findLocked(approvalID)
	if a == nil {
		return nil, fmt.Errorf("approval %s not found", approvalID)
	}
	if a.Status != ApprovalExecuting {
		return nil, fmt.Errorf("approval %s is %s; only EXECUTING approvals can complete", approvalID, a.Status)
	}
	if success {
		a.Status = ApprovalExecuted
	} else {
		a.Status = ApprovalFailed
	}
	a.VerificationResult = verification
	a.RollbackResult = rollback
	_ = s.save()
	evType := "approval_completed"
	if !success {
		evType = "approval_failed"
	}
	s.broadcast(ApprovalEvent{Type: evType, Approval: *a, At: s.now()})
	return a, nil
}

// CancelExpired transitions every PENDING approval past its expiry to
// EXPIRED and returns the ids it closed. The sweeper is idempotent.
func (s *ApprovalStore) CancelExpired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var closed []string
	for _, a := range s.approvals {
		if a.Pending() && a.ExpiredAt(now) {
			a.Status = ApprovalExpired
			closed = append(closed, a.ApprovalID)
			s.broadcast(ApprovalEvent{Type: "approval_expired", Approval: *a, At: now})
		}
	}
	if len(closed) > 0 {
		_ = s.save()
	}
	return closed
}

// History returns the decision trail for one approval id.
func (s *ApprovalStore) History(approvalID string) ([]ApprovalEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.findLocked(approvalID)
	if a == nil {
		return nil, fmt.Errorf("approval %s not found", approvalID)
	}
	var out []ApprovalEvent
	evt := func(typ string, at time.Time) {
		if at.IsZero() {
			return
		}
		out = append(out, ApprovalEvent{Type: typ, Approval: *a, At: at})
	}
	evt("approval_required", a.CreatedAt)
	if a.Status == ApprovalRejected {
		evt("approval_rejected", a.DecidedAt)
	} else {
		evt("approval_approved", a.DecidedAt)
	}
	evt("approval_executing", a.ExecutedAt)
	switch a.Status {
	case ApprovalExecuted:
		evt("approval_completed", a.ExecutedAt)
	case ApprovalFailed:
		evt("approval_failed", a.ExecutedAt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// save persists atomically; memory-only stores skip I/O.
func (s *ApprovalStore) save() error {
	if s.path == "" {
		return nil
	}
	wrapper := struct {
		Approvals []*Approval `json:"approvals"`
	}{Approvals: s.approvals}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteFileAtomic(s.path, append(data, '\n'), 0o600)
}

// HashAction computes the immutable action binding: SHA-256 over the
// canonical action payload (type + file names + content hashes). Any change
// to the proposed action changes the hash and invalidates prior approvals.
func HashAction(actionType string, files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("action:")
	b.WriteString(actionType)
	for _, n := range names {
		b.WriteString("\nfile:" + n + "\n" + HashString(files[n]))
	}
	return HashString(b.String())
}
