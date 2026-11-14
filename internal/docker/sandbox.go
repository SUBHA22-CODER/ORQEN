package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
)

// SandboxRunner validates proposed manifests in an isolated compose project.
// It implements safety.SandboxRunner so the Repair Safety Engine can reject
// invalid patches before they touch the production work directory, while all
// Docker invocation stays inside this package.
type SandboxRunner struct {
	once sync.Once
	v1   bool
}

// NewSandboxRunner returns the production sandbox runner.
func NewSandboxRunner() *SandboxRunner { return &SandboxRunner{} }

func (s *SandboxRunner) detect() {
	s.once.Do(func() {
		if _, err := exec.LookPath("docker"); err == nil {
			if err := exec.Command("docker", "compose", "version").Run(); err == nil {
				s.v1 = false
				return
			}
		}
		s.v1 = true
	})
}

func (s *SandboxRunner) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	s.detect()
	var bin string
	var full []string
	if s.v1 {
		bin = "docker-compose"
		full = []string{"-f", filepath.Join(dir, "docker-compose.yml")}
	} else {
		bin = "docker"
		full = []string{"compose", "-f", filepath.Join(dir, "docker-compose.yml")}
	}
	full = append(full, "-p", sandboxProjectName())
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	return cmd
}

func (s *SandboxRunner) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := s.command(ctx, dir, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// ConfigValidate parses and interpolates the proposed compose file (and its
// Dockerfile references) without touching any real project.
func (s *SandboxRunner) ConfigValidate(ctx context.Context, dir string) (string, error) {
	return s.run(ctx, dir, "config", "--quiet")
}

// Build builds the proposed service in the isolated project. Callers that do
// not enable sandbox builds never reach this.
func (s *SandboxRunner) Build(ctx context.Context, dir, service string) (string, error) {
	args := []string{"build"}
	if service != "" {
		args = append(args, service)
	}
	out, err := s.run(ctx, dir, args...)
	if err != nil {
		return out, fmt.Errorf("sandbox build: %w", err)
	}
	return out, nil
}

// sandboxProjectName generates a unique compose project name per invocation so
// concurrent sandbox validations never collide.
func sandboxProjectName() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "orqen-sandbox"
	}
	return "orqen-sandbox-" + hex.EncodeToString(b[:])
}
