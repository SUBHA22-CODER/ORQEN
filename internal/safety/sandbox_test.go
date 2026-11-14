package safety

import (
	"context"
	"strings"
	"testing"
)

type fakeSandboxRunner struct {
	configErr error
	configOut string
	buildErr  error
	buildOut  string
}

func (f *fakeSandboxRunner) ConfigValidate(ctx context.Context, dir string) (string, error) {
	return f.configOut, f.configErr
}

func (f *fakeSandboxRunner) Build(ctx context.Context, dir, service string) (string, error) {
	return f.buildOut, f.buildErr
}

func TestSandboxRejectsInvalidCompose(t *testing.T) {
	sb := &Sandbox{Runner: &fakeSandboxRunner{configErr: context.DeadlineExceeded, configOut: "invalid yaml: line 3"}}
	res := sb.Validate(context.Background(), Proposal{
		Files: map[string]string{"docker-compose.yml": "services: [oops\n"},
	})
	if res.Valid {
		t.Fatal("invalid compose must be rejected")
	}
	if res.Stage != "config" || !strings.Contains(res.Detail, "rejected") {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSandboxAcceptsValidPatch(t *testing.T) {
	sb := &Sandbox{Runner: &fakeSandboxRunner{}}
	res := sb.Validate(context.Background(), Proposal{
		Files: map[string]string{"Dockerfile.api": "FROM node:22\n", "docker-compose.yml": "services:\n  api: {}\n"},
	})
	if !res.Valid {
		t.Fatalf("valid patch rejected: %s", res.Detail)
	}
}

func TestSandboxWithoutRunnerPassesThrough(t *testing.T) {
	var sb *Sandbox
	res := sb.Validate(context.Background(), Proposal{Files: map[string]string{"docker-compose.yml": "x"}})
	if !res.Valid {
		t.Fatalf("nil sandbox must not reject: %s", res.Detail)
	}
}

func TestSandboxBuildUnavailableDowngrades(t *testing.T) {
	sb := &Sandbox{Runner: &fakeSandboxRunner{buildErr: ErrBuildUnavailable}, Build: true}
	res := sb.Validate(context.Background(), Proposal{Files: map[string]string{"docker-compose.yml": "services:\n  api: {}\n"}, Service: "api"})
	if !res.Valid {
		t.Fatalf("unavailable build must downgrade, not reject: %s", res.Detail)
	}
}

func TestSandboxBuildFailureRejects(t *testing.T) {
	sb := &Sandbox{Runner: &fakeSandboxRunner{buildErr: context.DeadlineExceeded, buildOut: "RUN npm ci failed"}, Build: true}
	res := sb.Validate(context.Background(), Proposal{Files: map[string]string{"docker-compose.yml": "services:\n  api: {}\n"}, Service: "api"})
	if res.Valid {
		t.Fatal("failed sandbox build must reject the patch")
	}
	if res.Stage != "build" {
		t.Fatalf("expected build stage, got %s", res.Stage)
	}
}
