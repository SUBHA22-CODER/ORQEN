package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// Approval demo mode: a deterministic, isolated demonstration of the judge
// HITL pipeline. Every event runs through the REAL gate, approval store,
// coordinator, and revalidation path — only the container stack is simulated
// and the "production" target is a temporary fixture directory. Records are
// labeled Simulated=true and use the reserved project "demo-simulated".

// Demo event names surfaced in the judge UI.
const (
	DemoLowRisk     = "LOW_RISK_FIX"
	DemoHighRisk    = "HIGH_RISK_FIX"
	DemoUncertain   = "UNCERTAIN_DIAGNOSIS"
	DemoDestructive = "DESTRUCTIVE_ACTION"
	demoProjectID   = "demo-simulated"
)

const demoComposeOriginal = "services:\n  api:\n    image: python:3.11-slim\n"

// demoStoreOverride, when set (tests, isolated demos), replaces the shared
// approval store so demo runs never touch the user's real approvals.json.
var demoStoreOverride *safety.ApprovalStore

func demoApprovalStore() *safety.ApprovalStore {
	if demoStoreOverride != nil {
		return demoStoreOverride
	}
	return demoApprovalStore()
}

// demoFixture is the isolated target the demo proposes changes against.
type demoFixture struct {
	Dir string
}

func newDemoFixture() (*demoFixture, error) {
	dir, err := os.MkdirTemp("", "orqen-demo-*")
	if err != nil {
		return nil, err
	}
	f := &demoFixture{Dir: dir}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(demoComposeOriginal), 0o600); err != nil {
		return nil, err
	}
	return f, nil
}

// applyFiles writes every proposed file into the fixture directory.
func (f *demoFixture) applyFiles(files map[string]string) error {
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(f.Dir, filepath.Base(name)), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *demoFixture) current() string {
	data, err := os.ReadFile(filepath.Join(f.Dir, "docker-compose.yml"))
	if err != nil {
		return ""
	}
	return string(data)
}

// contains reports whether any fixture file contains the token.
func (f *demoFixture) contains(token string) bool {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.Dir, e.Name()))
		if err == nil && strings.Contains(string(data), token) {
			return true
		}
	}
	return false
}

// scriptedDemoStack implements domain.ComposeController for the demo.
type scriptedDemoStack struct {
	mu   sync.Mutex
	pss  [][]domain.Container
	idx  int
	logs map[string]string
}

func (s *scriptedDemoStack) nextPs() []domain.Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx < len(s.pss) {
		p := s.pss[s.idx]
		s.idx++
		return p
	}
	if len(s.pss) == 0 {
		return nil
	}
	return s.pss[len(s.pss)-1]
}

func (s *scriptedDemoStack) Ps(ctx context.Context) ([]domain.Container, error) {
	return s.nextPs(), nil
}
func (s *scriptedDemoStack) Logs(ctx context.Context, service string, tail int) (string, error) {
	return s.logs[service], nil
}
func (s *scriptedDemoStack) Stats(ctx context.Context) ([]domain.ContainerStat, error) {
	return nil, nil
}
func (s *scriptedDemoStack) Build(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedDemoStack) Up(ctx context.Context, extra ...string) (string, error) { return "", nil }
func (s *scriptedDemoStack) Down(ctx context.Context, volumes bool) (string, error)  { return "", nil }
func (s *scriptedDemoStack) Restart(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedDemoStack) Stop(ctx context.Context, service string) (string, error) { return "", nil }
func (s *scriptedDemoStack) Start(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedDemoStack) Recreate(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedDemoStack) Available() bool { return true }

// demoSandbox always accepts: the fixture content is validated by the
// pipeline itself and the fixture is isolated, not real infrastructure.
type demoSandbox struct{}

func (demoSandbox) ConfigValidate(ctx context.Context, dir string) (string, error) { return "", nil }
func (demoSandbox) Build(ctx context.Context, dir, service string) (string, error) {
	return "", safety.ErrBuildUnavailable
}

// demoEventSpec describes one scripted demo event. The `files` field is the
// full proposed change set; the risk engine grades it independently, so a
// spec's intent (e.g. HIGH_RISK) is only realized when the proposed action
// genuinely grades that way — the engine never trusts scripted labels.
type demoEventSpec struct {
	name        string
	description string
	evidence    []string
	confidence  float64
	risk        safety.RiskLevel
	uncertain   bool
	diagnosis   string
	files       map[string]string
	verifyToken string
}

func demoSpecs() map[string]demoEventSpec {
	return map[string]demoEventSpec{
		DemoLowRisk: {
			name:        DemoLowRisk,
			description: "Deterministic low-risk fix with strong corroborated evidence: auto-executes without the judge",
			evidence: []string{
				"container_state[docker] api: PROBLEM (restart_loop:api:exit=1)",
				"log_tail[log] api: PROBLEM (log_connection_refused:api)",
			},
			confidence:  0.9,
			risk:        safety.RiskLow,
			diagnosis:   "lockfile desync in the generated build step",
			files:       map[string]string{"Dockerfile.api": demoComposeOriginal + "# demo fix: npm ci -> npm install --legacy-peer-deps\n"},
			verifyToken: "npm install --legacy-peer-deps",
		},
		DemoHighRisk: {
			name:        DemoHighRisk,
			description: "High blast-radius change (three files, two services): execution pauses until the judge approves",
			evidence: []string{
				"container_state[docker] api: PROBLEM (restart_loop:api:exit=1)",
				"dependency_state[docker] postgres: ok",
			},
			confidence: 0.85,
			risk:       safety.RiskHigh,
			diagnosis:  "api incompatibility requires recreating services with newer images",
			files: map[string]string{
				"docker-compose.yml": "services:\n  api:\n    image: python:3.12-slim\n  web:\n    image: node:24\n",
				"Dockerfile.api":     "FROM python:3.12-slim\n",
				"Dockerfile.web":     "FROM node:24\n",
			},
			verifyToken: "python:3.12-slim",
		},
		DemoUncertain: {
			name:        DemoUncertain,
			description: "Conflicting health signals: actionable but uncertain — the judge chooses investigation or repair",
			evidence: []string{
				"http_probe[probe] api: PROBLEM (http_application_error:api)",
				"container_state[docker] api: ok",
			},
			confidence:  0.5,
			risk:        safety.RiskLow,
			uncertain:   true,
			diagnosis:   "the api health endpoint returns 500; root cause unclear from current evidence",
			files:       map[string]string{"Dockerfile.api": "FROM python:3.12-slim\n"},
			verifyToken: "python:3.12-slim",
		},
		DemoDestructive: {
			name:        DemoDestructive,
			description: "Proposed repair contains data-loss operations: policy refuses even an explicit approval",
			evidence: []string{
				"container_state[docker] api: PROBLEM (restart_loop:api:exit=1)",
			},
			confidence:  0.8,
			risk:        safety.RiskCritical,
			diagnosis:   "reset the stack including its volumes to clear the failure",
			files:       map[string]string{"docker-compose.yml": demoComposeOriginal + "# demo: docker compose down -v\n"},
			verifyToken: "down -v",
		},
	}
}

// runApprovalDemo executes one scripted event through the real gate pipeline
// against the isolated fixture. Approval-gated events return immediately and
// finish in the background once the judge decides; the shared approval store
// carries the full lifecycle for the SSE stream and the audit trail.
func runApprovalDemo(event string) (map[string]any, error) {
	spec, ok := demoSpecs()[event]
	if !ok {
		return nil, fmt.Errorf("unknown demo event %q (expected one of: %s)", event, strings.Join(sortedDemoEvents(), ", "))
	}
	fixture, err := newDemoFixture()
	if err != nil {
		return nil, err
	}

	stack := &scriptedDemoStack{
		pss: [][]domain.Container{
			{{Service: "api", State: "restarting", ExitCode: 1}},
		},
		logs: map[string]string{"api": "error: connection refused\n"},
	}
	col := &safety.Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	verdict := safety.Verdict{
		Evidence:   buildDemoEvidence(spec),
		Transience: safety.PersistentFault,
		Signature:  "demo_" + strings.ToLower(event),
	}
	coord := &safety.ApprovalCoordinator{Store: demoApprovalStore(), ProjectID: demoProjectID, Simulated: true}
	gate := &safety.Gate{
		Collector:   col,
		Sampler:     &safety.Sampler{Collector: col, Samples: 2, Sleep: func(time.Duration) {}},
		Validator:   safety.Validator{},
		Sandbox:     &safety.Sandbox{Runner: demoSandbox{}},
		Coordinator: coord,
	}

	proposal := safety.Proposal{
		Files:   spec.files,
		Service: "api",
		Summary: spec.diagnosis,
	}
	diagnosis := safety.Diagnosis{Raw: spec.diagnosis, SourceKind: "demo"}
	actionType := "manifest_patch"
	if len(spec.files) > 1 {
		actionType = "multi_manifest"
	}
	actionHash := safety.HashAction(actionType, proposal.Files)

	report := map[string]any{
		"event":       event,
		"simulated":   true,
		"description": spec.description,
		"fixture":     fixture.Dir,
		"evidence":    spec.evidence,
		"confidence":  spec.confidence,
		"risk":        spec.risk.RiskLabel(),
		"uncertain":   spec.uncertain,
	}

	// Prohibited operations never execute — not even with an approval. The
	// request is created, revalidated, and failed for the audit trail.
	joined := ""
	for _, content := range spec.files {
		joined += content + "\n"
	}
	if markers := safety.Prohibited(joined); len(markers) > 0 {
		a := safety.Approval{
			ProjectID: demoProjectID, Simulated: true,
			ActionType: actionType, ActionSummary: spec.diagnosis,
			RiskLevel: spec.risk.RiskLabel(), ConfidenceScore: spec.confidence,
			ConfidenceTier: safety.ConfidenceTier(spec.confidence),
			Evidence:       spec.evidence,
			AffectedFiles:  fileNames(spec.files), AffectedServices: []string{"api"},
			Signature:  verdict.Signature,
			ActionHash: actionHash,
		}
		created, cerr := demoApprovalStore().Create(a)
		if cerr != nil {
			return nil, cerr
		}
		// The demo simulates the judge granting the approval, then shows the
		// execution stage refusing anyway: policy hard limits outrank any
		// approval.
		_, _ = demoApprovalStore().Decide(safety.ApprovalDecision{
			ApprovalID: created.ApprovalID, Decision: safety.DecisionRepair,
			Approved: true, ActionHash: actionHash,
			ProjectID: demoProjectID, DecidedBy: "simulated-judge",
		})
		_, _ = demoApprovalStore().Revalidate(created.ApprovalID, actionHash, demoProjectID)
		_, _ = demoApprovalStore().Complete(created.ApprovalID, false,
			"refused: prohibited operation ("+strings.Join(markers, "; ")+")",
			"nothing to roll back: no modification was ever applied")
		report["approval_id"] = created.ApprovalID
		report["decision"] = "refused_prohibited"
		report["outcome"] = "fixture untouched; prohibited action blocked even with an approval"
		return report, nil
	}

	// Review through the real gate. Low-risk auto-applies synchronously;
	// approval-gated events pause inside the coordinator until the judge
	// decides, so review happens in a goroutine and this handler returns.
	go func() {
		pd := gate.ReviewPlan(context.Background(), verdict, diagnosis, proposal)
		switch pd.Decision {
		case safety.AutoApply:
			_ = fixture.applyFiles(spec.files)
		default:
			// Blocked or refused decisions end here; the coordinator has
			// already recorded the approval lifecycle.
		}
	}()

	// Wait briefly for the gate to either finish or pause on an approval.
	var approvalID string
	for i := 0; i < 40; i++ { // up to 2s
		for _, a := range demoApprovalStore().List(safety.ApprovalPending, demoProjectID) {
			if a.ActionHash == actionHash {
				approvalID = a.ApprovalID
			}
		}
		if approvalID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if approvalID == "" {
		// No approval was created: the event resolved synchronously. Report
		// the fixture outcome (low-risk auto-apply path).
		changed := fixture.contains(spec.verifyToken)
		report["decision"] = "auto_apply"
		report["outcome"] = map[string]any{
			"executed":     changed,
			"verification": "fixture " + map[bool]string{true: "contains", false: "does not contain"}[changed] + " the patched content",
		}
		return report, nil
	}

	// Approval-gated: the pipeline is paused; the finisher completes the
	// lifecycle when the judge decides.
	// Capture the store once: finishDemo must not re-read the
	// (test-swappable) global after this function returns.
	demoStore := demoApprovalStore()
	go finishDemo(demoStore, approvalID, spec, fixture)
	report["approval_id"] = approvalID
	report["decision"] = "awaiting_judge"
	report["outcome"] = "execution paused; decide in the approvals panel"
	return report, nil
}

// finishDemo completes a paused demo pipeline after the judge rules. The
// coordinator already revalidated the action hash; if the ruling was
// repair-approved, the fixture patch applies and the lifecycle is completed.
func finishDemo(store *safety.ApprovalStore, approvalID string, spec demoEventSpec, fixture *demoFixture) {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		a, ok := store.Get(approvalID)
		if !ok {
			return
		}
		switch a.Status {
		case safety.ApprovalExecuting:
			// Approved-repair: the coordinator authorized the exact action.
			if aerr := fixture.applyFiles(spec.files); aerr != nil {
				_, _ = store.Complete(approvalID, false, "fixture apply failed: "+aerr.Error(), "fixture unchanged")
				return
			}
			if fixture.contains(spec.verifyToken) {
				_, _ = store.Complete(approvalID, true,
					"fixture verified: patched content present",
					"fixture snapshot retained for rollback")
			} else {
				_, _ = store.Complete(approvalID, false, "fixture verification failed", "fixture restored")
			}
			return
		case safety.ApprovalRejected, safety.ApprovalExpired, safety.ApprovalCancelled:
			return // nothing executes; the store already carries the ruling
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// buildDemoEvidence converts spec evidence lines into a deterministic
// evidence set for the gate.
func buildDemoEvidence(spec demoEventSpec) safety.Set {
	set := safety.Set{}
	for i, line := range spec.evidence {
		healthy := !strings.Contains(line, "PROBLEM")
		sig := ""
		if !healthy {
			sig = fmt.Sprintf("demo_evidence:%s:%d", strings.ToLower(spec.name), i)
		}
		kind, source := safety.KindContainer, safety.SourceDocker
		switch {
		case strings.Contains(line, "http_probe"):
			kind, source = safety.KindHTTP, safety.SourceProbe
		case strings.Contains(line, "dependency_state"):
			kind = safety.KindDependency
		case strings.Contains(line, "log_tail"):
			kind, source = safety.KindLog, safety.SourceLog
		}
		set.Add(safety.Evidence{
			ID: fmt.Sprintf("demo:%d", i), Kind: kind, Source: source,
			Service: "api", Healthy: healthy, Detail: line, Signature: sig,
		})
	}
	return set
}

// sortedDemoEvents lists demo event names in a stable order for the UI.
func sortedDemoEvents() []string {
	names := make([]string, 0, len(demoSpecs()))
	for n := range demoSpecs() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// fileNames lists the names of a proposed file map, sorted.
func fileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
