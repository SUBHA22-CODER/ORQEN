package golang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetect_ServerModeSubcommand(t *testing.T) {
	dir := t.TempDir()
	// Simulate a Cobra-based CLI with a persistent serve command (like ORQEN)
	files := map[string]string{
		"go.mod":       "module example.com/orqen\n\ngo 1.21\n",
		"main.go":      "package main\n\nfunc main() {}\n",
		"cmd/root.go":  "package cmd\n\nfunc Execute() {}\n",
		"cmd/serve.go": "package cmd\n\nimport \"net/http\"\n\nfunc runServe() {\n  _ = http.ListenAndServe(\":8420\", nil)\n}\nvar serveCmd = \"Use: serve\"\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	svc := Detect(".", dir)
	if svc == nil {
		t.Fatal("Detect returned nil")
	}
	if svc.Port != 8420 {
		t.Errorf("expected detected port 8420, got %d", svc.Port)
	}
	if !strings.Contains(svc.StartCmd, "serve") || !strings.Contains(svc.StartCmd, "0.0.0.0") || !strings.Contains(svc.StartCmd, "8420") {
		t.Errorf("expected persistent server start cmd with 0.0.0.0 and 8420, got %q", svc.StartCmd)
	}
}

func TestDetect_PureCLITool(t *testing.T) {
	dir := t.TempDir()
	// Pure CLI tool without server mode or HTTP usage
	files := map[string]string{
		"go.mod":      "module example.com/mycli\n\ngo 1.21\n",
		"main.go":     "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n",
		"cmd/root.go": "package cmd\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	svc := Detect(".", dir)
	if svc == nil {
		t.Fatal("Detect returned nil")
	}
	// For pure CLI, start command should remain standard ./app without serve args
	if svc.StartCmd != "./app" {
		t.Errorf("expected standard ./app for pure CLI, got %q", svc.StartCmd)
	}
}
