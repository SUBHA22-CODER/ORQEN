package healer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/safefs"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// scriptedCompose returns a different ps snapshot per call so tests can model
// transient faults and persistent crash loops.
type scriptedCompose struct {
	mu   sync.Mutex
	pss  [][]domain.Container
	idx  int
	logs map[string]string
}

func (s *scriptedCompose) nextPs() []domain.Container {
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

func (s *scriptedCompose) Ps(ctx context.Context) ([]domain.Container, error) { return s.nextPs(), nil }
func (s *scriptedCompose) Logs(ctx context.Context, service string, tail int) (string, error) {
	return s.logs[service], nil
}
func (s *scriptedCompose) Stats(ctx context.Context) ([]domain.ContainerStat, error) {
	return nil, nil
}
func (s *scriptedCompose) Build(ctx context.Context, service string) (string, error) { return "", nil }
func (s *scriptedCompose) Up(ctx context.Context, extra ...string) (string, error)   { return "", nil }
func (s *scriptedCompose) Down(ctx context.Context, volumes bool) (string, error)    { return "", nil }
func (s *scriptedCompose) Restart(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedCompose) Stop(ctx context.Context, service string) (string, error)  { return "", nil }
func (s *scriptedCompose) Start(ctx context.Context, service string) (string, error) { return "", nil }
func (s *scriptedCompose) Recreate(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (s *scriptedCompose) Available() bool { return true }

// fakeSbx implements safety.SandboxRunner without invoking Docker.
type fakeSbx struct{ err error }

func (f *fakeSbx) ConfigValidate(ctx context.Context, dir string) (string, error) {
	return "", f.err
}
func (f *fakeSbx) Build(ctx context.Context, dir, service string) (string, error) {
	return "", f.err
}

// TestHealWithGateTransientBlip: an isolated temporary error must not trigger
// any infrastructure modification.
func TestHealWithGateTransientBlip(t *testing.T) {
	work := t.TempDir()
	jail, err := safefs.NewJail(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := jail.WriteFile("Dockerfile.api", []byte("FROM python:3.11-slim\nRUN pip install .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := &scriptedCompose{
		pss: [][]domain.Container{
			{{Service: "api", State: "restarting", ExitCode: 1}},
			{{Service: "api", State: "running", Health: "healthy"}},
			{{Service: "api", State: "running", Health: "healthy"}},
		},
	}
	col := &safety.Collector{Compose: compose, Services: []*domain.Service{{Name: "api"}}}
	gate := &safety.Gate{
		Collector: col,
		Sampler:   &safety.Sampler{Collector: col, Samples: 3, Sleep: func(time.Duration) {}},
	}
	h := &Healer{
		Compose: compose, WorkJail: jail, Provenance: NewProvenance(""),
		Services: []*domain.Service{{Name: "api"}}, MaxTries: 1,
		Safety: gate,
	}
	before, _ := jail.ReadFile("Dockerfile.api")
	res, err := h.Heal(context.Background(), "", "")
	if err != nil {
		t.Fatalf("transient fault must not fail the heal: %v", err)
	}
	if !res.Success {
		t.Fatalf("transient recovery should be a successful no-op: %+v", res)
	}
	after, _ := jail.ReadFile("Dockerfile.api")
	if string(before) != string(after) {
		t.Fatal("no file may be modified for a transient fault")
	}
}

// TestHealWithGateWrongDiagnosisRejected: an incorrect AI diagnosis
// contradicted by evidence is rejected; the manifest stays untouched; the
// failure is audited and the breaker records it.
func TestHealWithGateWrongDiagnosisRejected(t *testing.T) {
	work := t.TempDir()
	jail, err := safefs.NewJail(work)
	if err != nil {
		t.Fatal(err)
	}
	original := "FROM python:3.11-slim\nRUN pip install .\n"
	if err := jail.WriteFile("Dockerfile.api", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// Persistent crash loop with healthy postgres; the model blames postgres.
	compose := &scriptedCompose{
		pss: [][]domain.Container{
			{
				{Service: "api", State: "restarting", ExitCode: 1},
				{Service: "postgres", State: "running", Health: "healthy"},
			},
		},
		logs: map[string]string{"api": "error: connection refused\ncrash\n"},
	}
	llm := &scriptedHealLLM{responses: []string{
		`{"diagnosis":"postgres is not running so the api cannot reach the database","changes":[{"operation":"replace_exact","file":"Dockerfile","anchor":"RUN pip install .","content":"RUN pip install requests"}],"summary":"restart postgres"}`,
		`{"diagnosis":"postgres is not running","changes":[{"operation":"replace_exact","file":"Dockerfile","anchor":"RUN pip install .","content":"RUN pip install requests"}],"summary":"retry"}`,
		`{"diagnosis":"postgres is not running","changes":[{"operation":"replace_exact","file":"Dockerfile","anchor":"RUN pip install .","content":"RUN pip install requests"}],"summary":"retry"}`,
	}}
	audit := safety.NewAudit("")
	col := &safety.Collector{Compose: compose, Services: []*domain.Service{{Name: "api"}}}
	gate := &safety.Gate{
		Collector: col,
		Sampler:   &safety.Sampler{Collector: col, Samples: 3, Sleep: func(time.Duration) {}},
		Validator: safety.Validator{},
		Sandbox:   &safety.Sandbox{Runner: &fakeSbx{}},
		Audit:     audit,
		Breaker:   safety.NewBreaker("", 3, time.Hour),
	}
	h := &Healer{
		Compose: compose, LLM: llm, WorkJail: jail, Provenance: NewProvenance(""),
		Services: []*domain.Service{{Name: "api"}}, MaxTries: 1,
		Safety: gate,
	}
	res, err := h.Heal(context.Background(), "", "")
	if err == nil {
		t.Fatalf("conflicting diagnosis must fail the heal: %+v", res)
	}
	if !errors.Is(err, domain.ErrUnfixable) {
		t.Fatalf("expected unfixable after repeated conflicts, got: %v", err)
	}
	got, _ := jail.ReadFile("Dockerfile.api")
	if string(got) != original {
		t.Fatalf("manifest must be untouched after rejected diagnosis:\n%s", got)
	}
	if llm.calls != 3 {
		t.Fatalf("expected 3 LLM feedback rounds, got %d", llm.calls)
	}
	// The audit trail must record the rejected review with conflicts.
	var reviewed *safety.Record
	for _, r := range audit.Records() {
		r := r
		if r.Phase == "review" && len(r.Conflicts) > 0 {
			reviewed = &r
		}
	}
	if reviewed == nil {
		t.Fatal("audit must contain a review record with conflicts")
	}
	if reviewed.Outcome != "rejected" || !strings.Contains(strings.Join(reviewed.Conflicts, " "), "postgres") {
		t.Fatalf("audit record wrong: %+v", reviewed)
	}
}

// TestHealWithGateProhibitedAndApproval: destructive (prohibited) patches are
// refused as invariant violations even with an approving judge — approval can
// never bypass policy hard limits. A risky-but-permitted repair (multi-file)
// still goes through the approval flow: approved means applied at commit
// time, and because the fake stack never verifies healthy, the healer rolls
// it back — proving the rollback path.
func TestHealWithGateProhibitedAndApproval(t *testing.T) {
	original := "services:\n  api:\n    image: python:3.11-slim\n"
	prohibited := "services:\n  api:\n    image: python:3.12-slim\n    # docker volume rm data\n"
	risky := "services:\n  api:\n    image: python:3.12-slim\n    healthcheck:\n      test: [\"CMD\", \"curl\", \"-f\", \"http://localhost:8000/health\"]\n"

	// Case 1: prohibited patch — refused even with an approver that accepts
	// everything, because ValidateReplacement (an invariant, pre-approval)
	// rejects it.
	{
		work := t.TempDir()
		jail, _ := safefs.NewJail(work)
		_ = jail.WriteFile("docker-compose.yml", []byte(original), 0o644)
		compose := &scriptedCompose{
			pss:  [][]domain.Container{{{Service: "api", State: "restarting", ExitCode: 1}}},
			logs: map[string]string{"api": "fatal error: bad config\n"},
		}
		patch := `{"diagnosis":"wipe volumes","compose":"` + composeJSONEscape(prohibited) + `","summary":"wipe"}`
		llm := &scriptedHealLLM{responses: []string{patch, patch, patch, patch, patch, patch}}
		col := &safety.Collector{Compose: compose, Services: []*domain.Service{{Name: "api"}}}
		gate := &safety.Gate{
			Collector: col,
			Sampler:   &safety.Sampler{Collector: col, Samples: 2, Sleep: func(time.Duration) {}},
			Validator: safety.Validator{},
			Sandbox:   &safety.Sandbox{Runner: &fakeSbx{}},
			Approver:  func(context.Context, safety.ApprovalRequest) bool { return true },
		}
		h := &Healer{
			Compose: compose, LLM: llm, WorkJail: jail, Provenance: NewProvenance(""),
			Services: []*domain.Service{{Name: "api"}}, MaxTries: 1,
			Safety: gate,
		}
		_, _ = h.Heal(context.Background(), "", "")
		got, _ := jail.ReadFile("docker-compose.yml")
		if string(got) != original {
			t.Fatalf("prohibited repair must never be applied even with approval:\n%s", got)
		}
	}

	// Case 2: risky-but-permitted patch — a two-file repair grades
	// MEDIUM_RISK, which routes to approval. Approved means applied at commit
	// time; failed verification rolls it back.
	run := func(approved bool) (string, string) {
		work := t.TempDir()
		jail, err := safefs.NewJail(work)
		if err != nil {
			t.Fatal(err)
		}
		if err := jail.WriteFile("docker-compose.yml", []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := jail.WriteFile("Dockerfile.api", []byte("FROM python:3.11-slim\nRUN pip install .\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		compose := &scriptedCompose{
			pss: [][]domain.Container{
				{{Service: "api", State: "restarting", ExitCode: 1}},
			},
			logs: map[string]string{"api": "fatal error: bad config\n"},
		}
		patch := `{"diagnosis":"api config is broken","compose":"` + composeJSONEscape(risky) +
			`","dockerfile":"` + composeJSONEscape("FROM python:3.12-slim\nRUN pip install .\n") +
			`","summary":"replace compose and dockerfile"}`
		llm := &scriptedHealLLM{responses: []string{patch, patch, patch, patch, patch, patch}}
		audit := safety.NewAudit("")
		col := &safety.Collector{Compose: compose, Services: []*domain.Service{{Name: "api"}}}
		gate := &safety.Gate{
			Collector: col,
			Sampler:   &safety.Sampler{Collector: col, Samples: 2, Sleep: func(time.Duration) {}},
			Validator: safety.Validator{},
			Sandbox:   &safety.Sandbox{Runner: &fakeSbx{}},
			Audit:     audit,
		}
		if approved {
			gate.Approver = func(context.Context, safety.ApprovalRequest) bool { return true }
		}
		h := &Healer{
			Compose: compose, LLM: llm, WorkJail: jail, Provenance: NewProvenance(""),
			Services: []*domain.Service{{Name: "api"}}, MaxTries: 1,
			Safety: gate,
		}
		_, _ = h.Heal(context.Background(), "", "")
		got, _ := jail.ReadFile("docker-compose.yml")
		phase := "rejected"
		for _, r := range audit.Records() {
			if r.Phase == "review" {
				phase = r.Outcome
			}
		}
		return string(got), phase
	}

	got, phase := run(false)
	if got != original {
		t.Fatalf("declined repair must change nothing:\n%s", got)
	}
	if phase != "rejected" {
		t.Fatalf("declined patch must be audited as rejected, got %q", phase)
	}

	got2, phase2 := run(true)
	if phase2 != "approved" {
		t.Fatalf("approved patch must be audited as approved, got %q", phase2)
	}
	// The patch was committed (hashes recorded), then rolled back because the
	// fake stack never verifies healthy — the manifest is back to original.
	if got2 != original {
		t.Fatalf("failed repairs must be rolled back where supported:\n%s", got2)
	}
}

// composeJSONEscape embeds a compose body into a JSON string literal.
func composeJSONEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
