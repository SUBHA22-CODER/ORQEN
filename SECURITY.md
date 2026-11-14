# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.4.x   | :white_check_mark: |
| < 0.4.0 | :x:                |

## Responsible Vulnerability Disclosure

Security and deterministic safety are core design principles of ORQEN. If you discover a security vulnerability, we appreciate your help in disclosing it responsibly.

**Please do NOT open a public GitHub issue for security vulnerabilities.**

Instead, report security issues directly to the author and maintainer:
- **Maintainer:** Subha Prasana Parida
- **GitHub:** [@SUBHA22-CODER](https://github.com/SUBHA22-CODER)
- **Security Contact:** Open a private security advisory on GitHub at [https://github.com/SUBHA22-CODER/ORQEN/security/advisories/new](https://github.com/SUBHA22-CODER/ORQEN/security/advisories/new).

Please include:
1. Description of the vulnerability and affected component.
2. Step-by-step reproduction instructions or proof-of-concept.
3. Potential impact and attack vectors.

You will receive an acknowledgment within 48 hours, followed by regular updates until a patch is released.

## Secret and Credential Management

1. **API Keys & Tokens**:
   - `GEMINI_API_KEY` and `XAI_API_KEY` are used exclusively for LLM provider communication.
   - Keys are kept in memory and never written to configuration files, logs, or UI outputs.
   - Sensitive keys are automatically masked (`[REDACTED]`) in error messages and API responses.
2. **GitHub Personal Access Tokens (`GITHUB_PAT`)**:
   - Required only for private repository cloning.
   - Never injected into user application containers or committed to state.
3. **Environment Isolation**:
   - Secrets defined in `.env` are strictly excluded from version control via `.gitignore`.
   - Never commit `.env` or production credentials.

## Autonomous Repair Safety Controls

ORQEN implements hard bounds on automated actions:
- **Independent Safety Triad**: Diagnostic confidence, repair risk, and evidence sufficiency are measured independently by deterministic algorithms.
- **Circuit Breaker**: Halts automated repair attempts after 3 consecutive failures to avoid thrashing.
- **Judge Authorization**: High-risk changes (volume deletions, network reconfigurations, multi-file patches) strictly require signed judge approval with action-hash binding and replay protection.
- **Dry-run Capabilities**: Diagnostic assessment can always be executed in read-only mode (`--dry-run`).
