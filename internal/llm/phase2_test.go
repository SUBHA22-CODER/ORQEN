package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Test J: Function/tool calling with Grok.
// Verifies endpoint construction without duplicate /v1, bearer auth, and response parsing.
func TestPhase2_TestJ_GrokToolCalls(t *testing.T) {
	var receivedPath string
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["tools"] == nil {
			t.Error("expected tools in request body")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices": [{
				"message": {
					"content": "Analyzing repository...",
					"tool_calls": [{
						"id": "call_123",
						"type": "function",
						"function": {
							"name": "scan_repository",
							"arguments": "{\"path\":\".\",\"max_services\":5}"
						}
					}]
				}
			}]
		}`)
	}))
	defer srv.Close()

	// BaseURL ending in /v1 (as xAI documented https://api.x.ai/v1)
	c, err := New(Config{
		Provider: "grok",
		Model:    "grok-4.7",
		APIKey:   "xai-mock-secret-key-999",
		BaseURL:  srv.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("failed to create grok client: %v", err)
	}

	resp, err := c.CallTools(context.Background(), "You are a DevOps assistant.", "Scan the repo", []Tool{
		{
			Name:        "scan_repository",
			Description: "Scan the repository",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTools failed: %v", err)
	}

	// Verify no duplicate /v1
	if receivedPath != "/v1/chat/completions" {
		t.Errorf("expected path /v1/chat/completions, got %q (duplicate /v1 check)", receivedPath)
	}
	if authHeader != "Bearer xai-mock-secret-key-999" {
		t.Errorf("expected Bearer token header, got %q", authHeader)
	}
	if resp.Text != "Analyzing repository..." {
		t.Errorf("text = %q, want 'Analyzing repository...'", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "scan_repository" {
		t.Errorf("tc.Name = %q, want 'scan_repository'", tc.Name)
	}
	if tc.Arguments["path"] != "." {
		t.Errorf("tc.Arguments[path] = %v, want '.'", tc.Arguments["path"])
	}
}

// Test J: Function/tool calling with Gemini.
func TestPhase2_TestJ_GeminiToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1beta/models/") || r.URL.Query().Get("key") != "gem-key-xyz" {
			t.Errorf("unexpected url %s", r.URL.String())
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["tools"] == nil {
			t.Error("expected tools in request")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"candidates": [{
				"content": {
					"parts": [
						{"text": "Running diagnostics"},
						{"functionCall": {"name": "diagnose_stack", "args": {"service": "api"}}}
					]
				}
			}]
		}`)
	}))
	defer srv.Close()

	c, err := New(Config{
		Provider: "gemini",
		Model:    "gemini-3.5-flash-lite",
		APIKey:   "gem-key-xyz",
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("failed to create gemini client: %v", err)
	}

	resp, err := c.CallTools(context.Background(), "sys", "diagnose", []Tool{
		{Name: "diagnose_stack", Description: "diagnose", Parameters: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	if resp.Text != "Running diagnostics" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "diagnose_stack" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// Test H: Invalid API key produces actionable error without leaking the key.
func TestPhase2_TestH_AuthErrorDoesNotLeakKey(t *testing.T) {
	secretKey := "super-secret-api-key-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid API key provided","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()

	c, err := New(Config{
		Provider: "grok",
		Model:    "grok-4.7",
		APIKey:   secretKey,
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Call(context.Background(), "sys", "hello")
	if err == nil {
		t.Fatal("expected authentication error")
	}
	errStr := err.Error()
	if strings.Contains(errStr, secretKey) {
		t.Fatalf("CRITICAL SECURITY DEFECT: error message leaked raw secret key: %s", errStr)
	}
	if !strings.Contains(errStr, "401") && !strings.Contains(errStr, "invalid_api_key") {
		t.Errorf("expected actionable error message indicating auth failure, got %q", errStr)
	}
}

// Opt-in Live LLM testing
func TestLiveLLM_GeminiAndGrok(t *testing.T) {
	if os.Getenv("ORQEN_TEST_LIVE_LLM") != "1" {
		t.Skip("skipping live LLM test; opt-in with ORQEN_TEST_LIVE_LLM=1")
	}

	// If Gemini key is set, run one small prompt
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		c, err := New(Config{Provider: "gemini", APIKey: key})
		if err == nil {
			resp, err := c.Call(context.Background(), "You are a test probe.", "Say PONG.")
			if err != nil {
				t.Logf("Live Gemini test warning: %v", err)
			} else {
				t.Logf("Live Gemini response received (length %d)", len(resp))
			}
		}
	}

	// If xAI key is set, run one small prompt
	if key := os.Getenv("XAI_API_KEY"); key != "" {
		c, err := New(Config{Provider: "grok", APIKey: key})
		if err == nil {
			resp, err := c.Call(context.Background(), "You are a test probe.", "Say PONG.")
			if err != nil {
				t.Logf("Live Grok test warning: %v", err)
			} else {
				t.Logf("Live Grok response received (length %d)", len(resp))
			}
		}
	}
}
