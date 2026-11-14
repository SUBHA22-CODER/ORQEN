package safety

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
)

// TestPreFlightTransientFault: an isolated temporary error must NOT trigger
// any modification — the verdict is NoAction and the evidence shows recovery.
func TestPreFlightTransientFault(t *testing.T) {
	stack := &fakeStack{ps: [][]domain.Container{
		{{Service: "api", State: "restarting", ExitCode: 1}},
		{{Service: "api", State: "running", Health: "healthy"}},
		{{Service: "api", State: "running", Health: "healthy"}},
	}}
	col := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	g := &Gate{
		Collector: col,
		Sampler:   &Sampler{Collector: col, Samples: 3, Sleep: func(time.Duration) {}},
	}
	v, err := g.PreFlight(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != NoAction {
		t.Fatalf("transient fault must be NoAction, got %s (%s)", v.Decision, v.Reason)
	}
	if v.Transience != TransientFault {
		t.Fatalf("expected transient classification, got %s", v.Transience)
	}
	if len(v.Evidence.Problems()) != 0 {
		t.Fatalf("final evidence must show recovery, got %v", v.Evidence.Signatures())
	}
	if v.Reason == "" {
		t.Fatal("verdict must carry a plain-language reason")
	}
}

// TestPreFlightHealthyStack: nothing to heal.
func TestPreFlightHealthyStack(t *testing.T) {
	stack := &fakeStack{ps: [][]domain.Container{
		{{Service: "api", State: "running", Health: "healthy"}},
	}}
	g := &Gate{
		Collector: &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}},
	}
	v, err := g.PreFlight(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != NoAction {
		t.Fatalf("healthy stack must be NoAction, got %s", v.Decision)
	}
	if v.Transience != TransientUnknown {
		t.Fatalf("healthy stack must not be classified transient, got %s", v.Transience)
	}
}

// TestPreFlightPersistentFaultCleared: a persistent, well-corroborated fault
// is cleared for the repair loop (Investigate = proceed autonomously).
func TestPreFlightPersistentFaultCleared(t *testing.T) {
	ps := []domain.Container{{Service: "api", State: "restarting", ExitCode: 1}}
	stack := &fakeStack{ps: [][]domain.Container{ps, ps, ps}}
	col := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	g := &Gate{
		Collector: col,
		Sampler:   &Sampler{Collector: col, Samples: 3, Sleep: func(time.Duration) {}},
	}
	v, err := g.PreFlight(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Transience != PersistentFault {
		t.Fatalf("expected persistent, got %s", v.Transience)
	}
	if v.Decision != Investigate {
		t.Fatalf("persistent fault must be cleared for the repair loop, got %s (%s)", v.Decision, v.Reason)
	}
	if v.Confidence < AutoApplyConfidence {
		t.Fatalf("corroborated persistent fault confidence %.2f too low", v.Confidence)
	}
	if v.Signature == "" {
		t.Fatal("persistent fault must expose a signature")
	}
}

// TestPreFlightOpenCircuitBlocks: after repeated failures the breaker pauses
// autonomous healing.
func TestPreFlightOpenCircuitBlocks(t *testing.T) {
	stack := &fakeStack{ps: [][]domain.Container{
		{{Service: "api", State: "restarting", ExitCode: 1}},
	}}
	br := NewBreaker("", 1, time.Hour)
	br.RecordOutcome("container_unhealthy:api", false, "boom")
	g := &Gate{
		Collector: &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}},
		Breaker:   br,
	}
	v, err := g.PreFlight(context.Background(), "container_unhealthy:api")
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != NoAction || !v.BreakerOpen {
		t.Fatalf("open circuit must yield NoAction/BreakerOpen, got %s open=%v (%s)", v.Decision, v.BreakerOpen, v.Reason)
	}
	if !strings.Contains(v.Reason, "circuit") {
		t.Fatalf("reason must explain the circuit: %s", v.Reason)
	}
}

// TestReviewPlanRejectsWrongDiagnosis: an incorrect AI diagnosis contradicted
// by deterministic evidence must be rejected.
func TestReviewPlanRejectsWrongDiagnosis(t *testing.T) {
	stack := &fakeStack{ps: [][]domain.Container{
		{{Service: "api", State: "restarting", ExitCode: 1}},
	}}
	// Hand-built evidence: postgres healthy, api crash-looping.
	ev := Set{}
	ev.Add(Evidence{ID: "container:postgres", Kind: KindContainer, Source: SourceDocker, Service: "postgres", Healthy: true})
	ev.Add(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"})
	verdict := Verdict{Evidence: ev, Transience: PersistentFault, Signature: "restart_loop:api:exit=1"}

	g := &Gate{
		Collector: &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}},
		Validator: Validator{},
	}
	d := Diagnosis{
		Raw:        "postgres is not running; the api fails because its database is unreachable",
		Claims:     ExtractClaims("postgres is not running"),
		SourceKind: "llm",
	}
	pd := g.ReviewPlan(context.Background(), verdict, d, Proposal{
		Files:   map[string]string{"docker-compose.yml": "services:\n  api: {}\n"},
		Service: "api",
		Summary: "restart postgres",
	})
	if pd.Decision != Investigate {
		t.Fatalf("wrong diagnosis must be rejected (Investigate), got %s", pd.Decision)
	}
	if len(pd.Conflicts) == 0 {
		t.Fatal("expected at least one conflict")
	}
	if !strings.Contains(pd.Explanation, "postgres") {
		t.Fatalf("explanation must name the contradiction: %s", pd.Explanation)
	}
}

// TestReviewPlanModelOnlyCapped: a diagnosis with zero deterministic support
// can never clear the autonomous bar.
func TestReviewPlanModelOnlyCapped(t *testing.T) {
	ev := Set{}
	ev.Add(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Healthy: true})
	verdict := Verdict{Evidence: ev, Transience: TransientUnknown}
	g := &Gate{Validator: Validator{}}
	pd := g.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "the api is misconfigured", Claims: nil, SourceKind: "llm"},
		Proposal{Files: map[string]string{"Dockerfile.api": "FROM node:22\n"}, Service: "api"})
	if pd.Decision == AutoApply {
		t.Fatalf("model-only diagnosis must never auto-apply (confidence %.2f)", pd.Confidence)
	}
	if pd.Confidence > llmOnlyCap {
		t.Fatalf("model-only confidence %.2f exceeds cap", pd.Confidence)
	}
}

// TestReviewPlanDestructiveRequiresApproval: a patch with data-loss markers
// must not run without approval.
func TestReviewPlanDestructiveRequiresApproval(t *testing.T) {
	ev := Set{}
	ev.Add(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"})
	verdict := Verdict{Evidence: ev, Transience: PersistentFault, Signature: "restart_loop:api:exit=1"}
	compose := "services:\n  db:\n    image: postgres\n# docker volume rm dbdata\n"

	// No approver: refused outright.
	g := &Gate{Validator: Validator{}}
	pd := g.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "", SourceKind: "llm"},
		Proposal{Files: map[string]string{"docker-compose.yml": compose}, Service: "db"})
	if pd.Decision != NoAction {
		t.Fatalf("destructive patch without approver must be refused, got %s (%s)", pd.Decision, pd.Explanation)
	}

	// With an approver that declines: still refused.
	declined := &Gate{Validator: Validator{}, Approver: func(context.Context, ApprovalRequest) bool { return false }}
	pd2 := declined.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "", SourceKind: "llm"},
		Proposal{Files: map[string]string{"docker-compose.yml": compose}, Service: "db"})
	if pd2.Decision != NoAction {
		t.Fatalf("declined approval must refuse, got %s", pd2.Decision)
	}

	// With an approver that accepts: approval-gated, marked as approved.
	accepted := &Gate{Validator: Validator{}, Approver: func(context.Context, ApprovalRequest) bool { return true }}
	pd3 := accepted.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "", SourceKind: "llm"},
		Proposal{Files: map[string]string{"docker-compose.yml": compose}, Service: "db"})
	if pd3.Decision != RequireApproval {
		t.Fatalf("destructive patch must stay approval-gated, got %s", pd3.Decision)
	}
	if !strings.Contains(pd3.Explanation, "approved") {
		t.Fatalf("approved explanation missing: %s", pd3.Explanation)
	}
}

// TestReviewPlanSandboxRejectsInvalidPatch: invalid patches are rejected
// before production application.
func TestReviewPlanSandboxRejectsInvalidPatch(t *testing.T) {
	ev := Set{}
	ev.Add(Evidence{ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api", Signature: "restart_loop:api:exit=1"})
	verdict := Verdict{Evidence: ev, Transience: PersistentFault, Signature: "restart_loop:api:exit=1"}
	g := &Gate{
		Validator: Validator{},
		Sandbox:   &Sandbox{Runner: &fakeSandboxRunner{configErr: context.DeadlineExceeded, configOut: "bad yaml"}},
	}
	pd := g.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "", SourceKind: "llm"},
		Proposal{Files: map[string]string{"docker-compose.yml": "services: [broken\n"}, Service: "api"})
	if pd.Decision != NoAction {
		t.Fatalf("sandbox-invalid patch must be NoAction, got %s", pd.Decision)
	}
	if pd.Sandbox == nil || pd.Sandbox.Valid {
		t.Fatal("sandbox result must be recorded in the decision")
	}
}

// TestGateAuditAndBreakerTrail: decisions are auditable with evidence hashes,
// and repeated failures open the circuit.
func TestGateAuditAndBreakerTrail(t *testing.T) {
	dir := t.TempDir()
	audit := NewAudit(filepath.Join(dir, "safety.json"))
	ps := []domain.Container{{Service: "api", State: "restarting", ExitCode: 1}}
	stack := &fakeStack{ps: [][]domain.Container{ps, ps, ps}}
	col := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	g := &Gate{
		Collector: col,
		Sampler:   &Sampler{Collector: col, Samples: 3, Sleep: func(time.Duration) {}},
		Validator: Validator{},
		Audit:     audit,
		Breaker:   NewBreaker("", 3, time.Hour),
	}
	v, err := g.PreFlight(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	recs := audit.Records()
	if len(recs) == 0 {
		t.Fatal("preflight must be audited")
	}
	if recs[0].Phase != "preflight" || recs[0].EvidenceHash == "" {
		t.Fatalf("audit record missing phase/evidence hash: %+v", recs[0])
	}

	// ReviewPlan appends a review record with patch hashes.
	verdict := v
	g.ReviewPlan(context.Background(), verdict,
		Diagnosis{Raw: "api is crashing", Claims: ExtractClaims("api is healthy"), SourceKind: "llm"},
		Proposal{Files: map[string]string{"Dockerfile.api": "FROM node:22\n"}, Service: "api"})
	found := false
	for _, r := range audit.Records() {
		if r.Phase == "review" && len(r.PatchHashes) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("review record with patch hashes missing")
	}

	// Three failed outcomes open the circuit.
	sig := v.Signature
	for i := 0; i < 3; i++ {
		g.RecordOutcome(sig, false, "repair failed")
	}
	if ok, _ := g.Breaker.Allow(sig); ok {
		t.Fatal("circuit must open after 3 failed outcomes")
	}
}
