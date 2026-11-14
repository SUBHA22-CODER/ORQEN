package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/state"
)

func TestMissingEnvKeys_SuppressesUnusedAlternativeProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ORQEN_HOME", home)
	t.Setenv("GEMINI_API_KEY", "valid-gemini-key")
	t.Setenv("ORQEN_LLM_PROVIDER", "gemini")

	mgrDir := filepath.Join(home, "projects", "proj1")
	if err := os.MkdirAll(mgrDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mgr := &state.Manager{ID: "proj1", Dir: mgrDir}

	lock := &domain.Lock{
		Project: "proj1",
		Env: []*domain.EnvVar{
			{Key: "GEMINI_API_KEY", Source: domain.EnvSourceExample, Required: true},
			{Key: "OPENAI_API_KEY", Source: domain.EnvSourceExample, Required: true},
			{Key: "ANTHROPIC_API_KEY", Source: domain.EnvSourceExample, Required: true},
			{Key: "GROQ_API_KEY", Source: domain.EnvSourceExample, Required: true},
			{Key: "XAI_API_KEY", Source: domain.EnvSourceExample, Required: true},
			{Key: "GITHUB_PAT", Source: domain.EnvSourceExample, Required: true},
			{Key: "DATABASE_URL", Source: domain.EnvSourceExample, Required: true},
			{Key: "OPTIONAL_DEBUG", Source: domain.EnvSourceExample, Required: false},
		},
	}

	// Save an initial env with only the user's configured provider key
	if err := mgr.SaveEnv(map[string]string{
		"GEMINI_API_KEY": "valid-gemini-key",
	}); err != nil {
		t.Fatal(err)
	}

	missing := missingEnvKeys(mgr, lock)

	// We expect ONLY DATABASE_URL to be reported as missing!
	// OPENAI_API_KEY, ANTHROPIC_API_KEY, GROQ_API_KEY, XAI_API_KEY, GITHUB_PAT, OPTIONAL_DEBUG must NOT be reported.
	if len(missing) != 1 || missing[0] != "DATABASE_URL" {
		t.Fatalf("expected missing=[DATABASE_URL], got %v", missing)
	}
}

func TestMissingEnvKeys_PreservesGenuineAppSpecificKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ORQEN_HOME", home)

	mgrDir := filepath.Join(home, "projects", "openai-app")
	if err := os.MkdirAll(mgrDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mgr := &state.Manager{ID: "openai-app", Dir: mgrDir}

	// An app that specifically requires OPENAI_API_KEY in its code
	lock := &domain.Lock{
		Project: "openai-app",
		Env: []*domain.EnvVar{
			{Key: "OPENAI_API_KEY", Source: domain.EnvSourceCode, Required: true},
			{Key: "STRIPE_SECRET_KEY", Source: domain.EnvSourceCode, Required: true},
		},
	}

	missing := missingEnvKeys(mgr, lock)
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing keys (OPENAI_API_KEY and STRIPE_SECRET_KEY), got %v", missing)
	}
	foundOpenAI := false
	for _, k := range missing {
		if k == "OPENAI_API_KEY" {
			foundOpenAI = true
		}
	}
	if !foundOpenAI {
		t.Errorf("genuine app key OPENAI_API_KEY should be reported as missing when required by code: got %v", missing)
	}
}
