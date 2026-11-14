package safety

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Proposal is a set of proposed manifest changes to validate in isolation
// before anything is written to the production work directory.
type Proposal struct {
	// Files maps writable manifest names (Dockerfile.<svc>,
	// docker-compose.yml) to their full proposed content.
	Files map[string]string `json:"files"`
	// Service is the primary affected service ("" when stack-wide).
	Service string `json:"service,omitempty"`
	// Summary is the model's stated intent, kept for the audit trail only.
	Summary string `json:"summary,omitempty"`
}

// SandboxResult is the outcome of isolated validation.
type SandboxResult struct {
	// Valid is true when the proposal passed every enabled check.
	Valid bool `json:"valid"`
	// Detail explains what was checked and what failed.
	Detail string `json:"detail"`
	// Stage names the check that failed (config | build | ""), for audits.
	Stage string `json:"stage,omitempty"`
}

// SandboxRunner is the isolated-execution seam. The production implementation
// shells out to docker compose inside a temporary project directory; tests
// substitute fakes. It receives the directory containing the proposed
// manifests and returns combined output and an error.
type SandboxRunner interface {
	// ConfigValidate runs compose config validation for the project at dir.
	ConfigValidate(ctx context.Context, dir string) (string, error)
	// Build optionally builds the given service (or all when empty) in the
	// isolated project. Implementations may return ErrBuildUnavailable to
	// signal that sandbox builds are not supported, which downgrades to
	// config-only validation instead of failing the proposal.
	Build(ctx context.Context, dir, service string) (string, error)
}

// ErrBuildUnavailable may be returned by SandboxRunner.Build to skip the
// optional build stage.
var ErrBuildUnavailable = fmt.Errorf("sandbox build unavailable")

// Sandbox validates proposed manifest changes in an isolated environment.
type Sandbox struct {
	Runner SandboxRunner
	// Build enables the (slow) sandbox build stage; config validation always
	// runs. Off by default.
	Build bool
	// Dir is the parent directory for isolated sandbox projects (defaults to
	// os.TempDir()).
	Dir string
}

// Validate runs the enabled checks against the proposal. It never modifies
// the proposal and never touches the production work directory: everything
// happens in a fresh temporary directory that is removed afterwards.
func (s *Sandbox) Validate(ctx context.Context, p Proposal) SandboxResult {
	if s == nil || s.Runner == nil {
		return SandboxResult{Valid: true, Detail: "sandbox not configured; static invariants remain the only gate"}
	}
	if len(p.Files) == 0 {
		return SandboxResult{Valid: true, Detail: "no files to validate"}
	}
	dir, err := os.MkdirTemp(s.sandboxRoot(), "orqen-sandbox-")
	if err != nil {
		return SandboxResult{Valid: false, Stage: "config", Detail: "create sandbox dir: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	names := make([]string, 0, len(p.Files))
	for n := range p.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(target, []byte(p.Files[name]), 0o644); err != nil {
			return SandboxResult{Valid: false, Stage: "config", Detail: "write sandbox manifest: " + err.Error()}
		}
	}

	// Stage 1 (always): compose config validation. This parses and
	// interpolates the compose file and references the Dockerfiles, catching
	// invalid YAML, missing services, and broken references before production.
	out, err := s.Runner.ConfigValidate(ctx, dir)
	if err != nil {
		return SandboxResult{
			Valid:  false,
			Stage:  "config",
			Detail: fmt.Sprintf("compose config rejected the patch: %s", Tail(strings.TrimSpace(out), 12)),
		}
	}

	// Stage 2 (optional): build the affected service in isolation.
	if s.Build {
		out, berr := s.Runner.Build(ctx, dir, p.Service)
		if berr != nil && berr != ErrBuildUnavailable {
			return SandboxResult{
				Valid:  false,
				Stage:  "build",
				Detail: fmt.Sprintf("sandbox build failed: %s", Tail(strings.TrimSpace(out), 12)),
			}
		}
		detail := "compose config valid; sandbox build skipped (unavailable)"
		if berr == nil {
			detail = "compose config valid; sandbox build succeeded"
		}
		return SandboxResult{Valid: true, Detail: detail}
	}
	return SandboxResult{Valid: true, Detail: "compose config valid in isolated sandbox"}
}

func (s *Sandbox) sandboxRoot() string {
	if s.Dir != "" {
		return s.Dir
	}
	return os.TempDir()
}
