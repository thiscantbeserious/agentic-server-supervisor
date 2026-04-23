# Agentic Server Supervisor

A robust, model-agnostic server monitoring and health-check system built on the **ZeroClaw** agent framework. This sentinel is designed to autonomously monitor system resources, analyze logs, and escalate alerts across multiple LLM providers.

## 🏗️ Architecture

- **Engine:** [ZeroClaw](https://github.com/zeroclaw-labs/zeroclaw) (High-performance Rust agent framework).
- **Control Logic:** Managed via Standard Operating Procedures (SOPs) located in `workspace/SOPs/`.
- **Security:** Strict command whitelisting and filesystem scoping defined in `config.toml`.
- **Autonomy:** `Supervised` mode—critical or medium-risk actions require human approval.

## 🚀 Quick Start

### 1. Bootstrap
Run the bootstrap script to install ZeroClaw and prepare the local environment:

```bash
./bootstrap.sh
```

### 2. Configure (Onboarding)
Finalize your AI provider setup (API keys, models, etc.). This command will detect the existing `config.toml` and `workspace` folder:

```bash
zeroclaw onboard --config-dir $(pwd)
```

### 3. Run the Sentinel
Start the agent in interactive mode to verify connectivity:

```bash
zeroclaw agent --config-dir $(pwd)
```

To run as a long-running daemon (with cron and heartbeat enabled):

```bash
zeroclaw daemon --config-dir $(pwd)
```

## 🛡️ Security Configuration

The system is configured with a strict security boundary in `config.toml`:

- **Allowed Commands:** `df`, `free`, `uptime`, `journalctl`, `git`, `ss`, `ip`, etc.
- **Allowed Roots:** Read-only access to `/etc/gemini-watcher/system/` and local workspace.
- **Forbidden Paths:** Sensitive areas like `/etc/shadow`, `~/.ssh`, and `/root` are hard-blocked.

## 📂 Project Structure

- `config.toml`: Central agent configuration and security policies.
- `workspace/`: The agent's persistent home (SOPs, memory, sessions).
- `workspace/SOPs/`: MarkDown-based procedures (e.g., `health-check.md`).
- `ARCHITECTURE.md`: Detailed design goals and migration paths.
