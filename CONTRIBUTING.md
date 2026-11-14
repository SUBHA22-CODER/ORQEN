# Contributing to ORQEN

Thank you for your interest in contributing to **ORQEN**!

ORQEN is maintained by **Subha Prasana Parida** ([@SUBHA22-CODER](https://github.com/SUBHA22-CODER)).

---

## Code of Conduct

All contributors and participants are expected to maintain a welcoming, respectful, and collaborative environment.

---

## Development Setup

### Prerequisites

- **Go 1.27+** installed and available in your `PATH`.
- **Docker Engine** (v28.0+) and **Docker Compose v2** (`docker compose`).
- **Git** (v2.30+).
- An API key for either **Google Gemini** (`GEMINI_API_KEY`) or **xAI Grok** (`XAI_API_KEY`).

### Clone and Build

```bash
git clone https://github.com/SUBHA22-CODER/ORQEN.git
cd ORQEN

# Verify dependencies
go mod verify

# Build binary
go build -o orqen.exe .

# Verify environment
./orqen.exe doctor
```

---

## Branch Naming & Commit Conventions

### Branch Naming
- `feat/<feature-name>`: New features or capabilities
- `fix/<bug-name>`: Bug fixes
- `docs/<topic>`: Documentation enhancements
- `test/<test-suite>`: Additional unit or integration tests
- `refactor/<target>`: Code cleanup without behavior alterations

### Commit Messages
Follow the Conventional Commits specification:
```
<type>(<scope>): <short description>

[optional body]
```
Examples:
- `feat(llm): add xAI Grok provider support`
- `fix(scanner): correct Go serve subcommand detection`
- `docs(readme): update safety score architecture diagram`

---

## Testing & Quality Requirements

Before opening a pull request, ensure all tests and quality checks pass:

```powershell
# 1. Format code
gofmt -w .
gofmt -l .

# 2. Static analysis
go vet ./...

# 3. Unit and integration tests (uncached)
go test -count=1 ./...

# 4. Deterministic safety verification
./orqen.exe safety-test
```

### Safety Engine Invariants
If modifying `internal/safety` or `internal/healer`:
- Diagnostic confidence, repair risk, and evidence sufficiency must remain computed **independently**.
- Never allow an LLM to self-score its confidence or override policy decisions.
- Never bypass confirmation checks or the action-hash-bound judge approval mechanism.

---

## Pull Request Guidelines

1. Fork the repository and create your branch from `main`.
2. Keep pull requests focused on a single logical change.
3. Include tests covering any new behavior or bug fixes.
4. Ensure your PR description clearly explains the motivation, implementation, and verification steps.
5. All CI checks must pass before merging.

---

## Reporting Vulnerabilities

For security-sensitive matters, please consult our [Security Policy](SECURITY.md) rather than posting publicly.
