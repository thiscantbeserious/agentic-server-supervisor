# Agentic Server Supervisor (Sentinel)

An opinionated, autonomous supervisor node implementation built on the **ZeroClaw** framework. This project transforms a standard server into an intelligent, self-monitoring node capable of real-time resource analysis, log investigation, and autonomous alerting.

Currently in **MVP state**, focusing on robust local execution and secure system access.

## 🎯 Intent & Vision

This is not just an agent; it is a dedicated **Supervisor Node**. 
- **Current Foundation:** Based on the ZeroClaw engine for high-performance Rust execution.
- **Opinionated Setup:** Pre-configured security boundaries, whitelisted monitoring tools, and read-only system-wide analysis.
- **Management:** Controlled via a dedicated Rust-based CLI (`sentinel-cli`).
- **Roadmap:** Future iterations will include a centralized management dashboard and a cross-agent mesh network for multi-node orchestration.

## 🏗️ Architecture

- **Core:** [ZeroClaw](https://github.com/zeroclaw-labs/zeroclaw) (Model-agnostic AI runtime).
- **Automation:** `Full` autonomy mode — the node acts independently within its whitelisted policy.
- **Security:** Hardware-specific identity (`hostname[ip]`), strict command whitelisting, and read-only access to system roots.
- **Tooling:** Automated bootstrapping using `cargo-binstall` and template-driven AI discovery via `minijinja`.

## 🚀 Deployment & Interactive Setup

### 1. Bootstrap the Node
Run the central deployment engine to install the environment and the `sentinel-cli` management tool:

```bash
./bootstrap.sh
```

### 2. Interactive AI Discovery
The bootstrap process isn't just an installer; it's an **intelligent provisioning flow**:

- **AI System Scan:** ZeroClaw autonomously probes your specific OS to find relevant monitoring tools (iostat, vmstat, sensors, etc.).
- **AI Filesystem Policy:** The agent scans for standard log/config paths and identifies highly sensitive directories to block.
- **TUI Approval:** Every suggestion (commands, allowed roots, forbidden paths) is presented in a visual checklist. **You have the final say** before any policy is written to disk.

## 🛠️ Node Management

All node operations are managed through the native `sentinel-cli`:

```bash
sentinel-cli status   # Check service health
sentinel-cli logs     # Tail active supervisor logs
sentinel-cli restart  # Hot-reload configuration and restart service
```

## 📂 Project Structure

- `bootstrap.sh`: The automated deployment and discovery engine.
- `sentinel-cli/`: Rust-based management utility.
- `config.toml`: Master security policy (read-only system roots, whitelisted commands).
- `prompts/`: MiniJinja templates for autonomous tool and folder discovery.
- `workspace/`: Persistent node identity, SOPs, and role definitions.
  - `memory/` / `sessions/`: Confidential runtime data (strictly Git-ignored).
