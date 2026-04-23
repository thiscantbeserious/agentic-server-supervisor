# Agentic Server Supervisor

A state-of-the-art, autonomous server monitoring system built on the **ZeroClaw** Rust framework. This supervisor (Server Sentinel) autonomously monitors resources, detects anomalies using AI, and manages alerts across any LLM provider (Gemini, Anthropic, OpenAI, etc.).

## 🏗️ Architecture

- **Engine:** [ZeroClaw](https://github.com/zeroclaw-labs/zeroclaw) — High-performance, memory-safe Rust agent.
- **Bootstrapping:** Automated via `cargo-binstall` for lightning-fast setup (Option A: Build from source fallback).
- **Configuration:** Managed robustly via `tomato-toml` (preserves comments and formatting).
- **AI Detection:** Integrated **MiniJinja** templating for context-aware autonomous tool discovery.
- **Autonomy:** `Full` mode — The Sentinel acts independently within strict security boundaries.
- **Security:** Hardware-specific identity, command whitelisting, and filesystem scoping.

## 🚀 Quick Start

### 1. One-Step Bootstrap
Run the enhanced bootstrap script. It will install Rust, ZeroClaw, and the required TUI tools automatically:

```bash
./bootstrap.sh
```

### 2. What happens during Bootstrap?
- **Identity:** Auto-detects `hostname[ip]` to give the sentinel a unique name.
- **Onboarding:** Interactive wizard to set your AI provider (uses Gemini CLI credentials by default).
- **AI System Scan:** (Optional) ZeroClaw autonomously scans your specific OS to find relevant monitoring tools.
- **Whitelist Approval:** You get the final say on which commands the agent is allowed to execute via a TUI checklist.
- **Daemonization:** Optionally installs the sentinel as a systemd user service.

## 🛡️ Security Policy

Configured in `config.toml` (which is ignored by Git to prevent leakage):

- **Whitelisted Commands:** Strictly limited to monitoring tools (`df`, `free`, `iostat`, etc.).
- **Filesystem Scoping:** Read-only access to `/etc/gemini-watcher/system/` and the local workspace.
- **Confidentiality:** Runtime data (memory, sessions, state) is strictly excluded from version control.

## 📂 Project Structure

- `bootstrap.sh`: The central setup entry point.
- `config.toml`: Master configuration and security gatekeeper.
- `prompts/`: Jinja2 templates for AI interactions (e.g., `tool-autodetection.prompt`).
- `workspace/`: 
  - `AGENTS.md`: Role and personality definition.
  - `SOPs/`: MarkDown-based monitoring procedures.
  - `IDENTITY.md` / `SOUL.md`: Local machine identity (tracked).
  - `memory/` / `sessions/`: Confidential runtime data (ignored).

## 🛠️ Management

Manage the sentinel service using the custom management CLI:

```bash
sentinel status
sentinel logs
sentinel restart
```

*(Future: Use `sentinel mesh` for cross-agent communication)*
