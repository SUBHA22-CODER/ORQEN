package config

import (
	"strings"
	"testing"
)

// Test A: Only GEMINI_API_KEY configured. Expected: Gemini resolves successfully.
func TestPhase2_TestA_OnlyGeminiConfigured(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "gemini-secret-test-key")

	r, err := Resolve()
	if err != nil {
		t.Fatalf("expected resolution to succeed, got %v", err)
	}
	if r.Provider != "gemini" {
		t.Errorf("expected provider gemini, got %q", r.Provider)
	}
	if r.APIKey != "gemini-secret-test-key" {
		t.Errorf("expected api key gemini-secret-test-key, got %q", r.APIKey)
	}
	if r.Model != DefaultModel("gemini") {
		t.Errorf("expected default model %q, got %q", DefaultModel("gemini"), r.Model)
	}
}

// Test B: Only XAI_API_KEY configured. Expected: Grok resolves successfully.
func TestPhase2_TestB_OnlyGrokConfigured(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "xai-secret-test-key")

	r, err := Resolve()
	if err != nil {
		t.Fatalf("expected resolution to succeed, got %v", err)
	}
	if r.Provider != "grok" {
		t.Errorf("expected provider grok, got %q", r.Provider)
	}
	if r.APIKey != "xai-secret-test-key" {
		t.Errorf("expected api key xai-secret-test-key, got %q", r.APIKey)
	}
	if r.Model != DefaultModel("grok") {
		t.Errorf("expected default model %q, got %q", DefaultModel("grok"), r.Model)
	}
}

// Test C: Both keys configured. Expected: Selected primary provider resolves correctly.
func TestPhase2_TestC_BothKeysConfigured_RespectPrimary(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("XAI_API_KEY", "xai-key")

	// Case 1: Grok explicitly selected
	t.Setenv("ORQEN_LLM_PROVIDER", "grok")
	rGrok, err := Resolve()
	if err != nil {
		t.Fatalf("resolve grok: %v", err)
	}
	if rGrok.Provider != "grok" || rGrok.APIKey != "xai-key" {
		t.Errorf("expected grok with xai-key, got provider=%q key=%q", rGrok.Provider, rGrok.APIKey)
	}

	// Case 2: Gemini explicitly selected
	t.Setenv("ORQEN_LLM_PROVIDER", "gemini")
	rGem, err := Resolve()
	if err != nil {
		t.Fatalf("resolve gemini: %v", err)
	}
	if rGem.Provider != "gemini" || rGem.APIKey != "gemini-key" {
		t.Errorf("expected gemini with gemini-key, got provider=%q key=%q", rGem.Provider, rGem.APIKey)
	}
}

// Test D: Only Gemini selected, no Grok key. Expected: No Grok credential warning / failure.
func TestPhase2_TestD_OnlyGeminiSelected_NoGrokKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("ORQEN_LLM_PROVIDER", "gemini")
	t.Setenv("GEMINI_API_KEY", "gemini-key")

	r, err := Resolve()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if r.Provider != "gemini" {
		t.Errorf("expected gemini, got %q", r.Provider)
	}
}

// Test E: Only Grok selected, no Gemini key. Expected: No Gemini credential warning / failure.
func TestPhase2_TestE_OnlyGrokSelected_NoGeminiKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("ORQEN_LLM_PROVIDER", "grok")
	t.Setenv("XAI_API_KEY", "xai-key")

	r, err := Resolve()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if r.Provider != "grok" {
		t.Errorf("expected grok, got %q", r.Provider)
	}
}

// Test F: Neither key configured. Expected: Clear setup instructions.
func TestPhase2_TestF_NeitherKeyConfigured(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())

	_, err := Resolve()
	if err == nil {
		t.Fatal("expected error when neither key is configured")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no LLM provider configured") || !strings.Contains(msg, "orqen setup") {
		t.Errorf("expected clear setup instructions in error message, got %q", msg)
	}
}

// Test G: Incorrect provider name. Expected: Friendly validation error.
func TestPhase2_TestG_IncorrectProviderName(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("ORQEN_LLM_PROVIDER", "invalid-provider-xyz")

	_, err := Resolve()
	if err == nil {
		t.Fatal("expected error for invalid provider")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown provider") || !strings.Contains(msg, "gemini") || !strings.Contains(msg, "grok") {
		t.Errorf("expected friendly error mentioning supported providers, got %q", msg)
	}
}

// Test H: Missing key for provider does not leak secrets.
func TestPhase2_TestH_MissingKeyValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("ORQEN_HOME", t.TempDir())
	t.Setenv("ORQEN_LLM_PROVIDER", "grok")

	_, err := Resolve()
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "XAI_API_KEY") {
		t.Errorf("expected error to mention XAI_API_KEY, got %q", msg)
	}
}
