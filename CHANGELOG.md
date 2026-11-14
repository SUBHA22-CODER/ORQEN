# Changelog

All notable changes to **ORQEN** are documented in this file.

## [v0.5.0] - 2026-11-14

### Supported LLM Provider Migration & Normalization
- **Gemini & xAI Grok Exclusive Support**: Streamlined user-facing LLM configurations to Google Gemini (`GEMINI_API_KEY`) and xAI Grok (`XAI_API_KEY`, default model `grok-4.7`).
- **Endpoint URL Normalization**: Corrected OpenAI-compatible URL construction in `internal/llm/providers.go` to prevent duplicate `/v1` segments (`https://api.x.ai/v1/chat/completions`).
- **Obsolete Provider Cleanup**: Removed deprecated OpenAI, Anthropic, and Groq prompts from CLI wizards and configuration panels.
- **Secret Sanitization**: Ensured API keys are never leaked in logs, error traces, or configuration outputs.

### Environment Discovery & False-Positive Elimination
- **Missing Variable Filtering**: Separated ORQEN's runtime LLM configuration from target repository application variables.
- **Required Flag Adherence**: Fixed `missingEnvKeys` in `cmd/common.go` to respect `ev.Required` and suppress unselected provider placeholders present in discovered `.env.example` files.
- **Optional GitHub PAT**: Public repository cloning no longer demands `GITHUB_PAT`. Tokens are isolated from application containers.

### Non-Bypass Onboarding Approval Flow
- **Three-State Approval Security**: Implemented explicit handling for Approval Granted, Approval Denied, and Approval Callback Unavailable in `internal/agent/intent.go`.
- **Web UI Modal Fix**: Fixed modal visibility in `internal/web/assets/app.js` (`els.modal.hidden = false/true`) and added interactive confirmation prompts for CLI onboarding.
- **Retry Support**: Denied onboardings record permission state cleanly and permit user retry without state corruption.

### Go Container Restart Loop & Clean Exit Diagnostics
- **Persistent Server Command Detection**: Enhanced `internal/scanner/golang/golang.go` to scan subcommands (e.g., `cmd/serve.go`), extracting listening ports (`8420`) and generating persistent commands: `./app serve --host 0.0.0.0 --port <PORT> --no-open`.
- **Exec-Form Dockerfile Generation**: Updated `internal/generator/dockerfile.go` to output valid exec-form JSON arrays `CMD ["/app/app", "serve", ...]`, preventing immediate exit 0.
- **Runtime Diagnostic Segregation**: Distinguished between clean-exit restart loops (`exit code 0`), OOM kills, and application crashes (`exit code > 0`), reporting:
  *"The container process completed successfully but the configured restart policy repeatedly restarted it. Inspect the startup command and service mode."*

### Repair Safety Engine & Deterministic Scoring
- **Independent Safety Triad**: Separated diagnostic confidence, repair risk, and evidence sufficiency into distinct, deterministic scores in `internal/safety/`.
- **Evidence Collector**: Cross-references Docker container state, HTTP probes, restart counts, and logs before proposing repairs.
- **Sandbox Pre-flight Validation**: Patches are validated with `docker compose config` in an isolated project sandbox prior to production application.
- **Circuit Breaker**: Implemented half-open cooldown and failure trip thresholds preventing thrashing.

### Human-in-the-Loop Judge Approval System
- **Action-Hash Binding**: Approvals are cryptographically bound to the specific patch SHA-256 and pending request ID.
- **Replay Protection & Expiration**: Enforced one-time use tokens and time-bound authorization windows.
- **Natural Language Parsing Guard**: Strict policy ensures hypothetical questions or chit-chat in natural language are never misinterpreted as approval.

### Authorship and Official Release
- Transferred repository attribution and copyright to **Subha Prasana Parida** ([@SUBHA22-CODER](https://github.com/SUBHA22-CODER)).
