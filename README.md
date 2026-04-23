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
- **Security:** Hardware-specific identity (`hostname[ip]`), strict command whitelisting, and read-only access to `/etc` and `/var/log`.
- **Tooling:** Automated bootstrapping using `cargo-binstall` and template-driven AI tool discovery via `minijinja`.

## 🚀 Deployment

### 1. Bootstrap the Node
Run the bootstrap script to install the environment and the `sentinel-cli` management tool:

```bash
./bootstrap.sh
```

### 2. Autonomous Capabilities
During bootstrap, the Sentinel performs an **AI-driven system scan** to detect relevant monitoring tools (iostat, vmstat, etc.) and presents a final whitelist for your approval.

## 🛠️ Node Management

All node operations are managed through `sentinel-cli`:

```bash
sentinel-cli status   # Check service health
sentinel-cli logs     # Tail active logs
sentinel-cli restart  # Hot-reload configuration
```

## 📂 Project Structure

- `bootstrap.sh`: The automated deployment engine.
- `sentinel-cli/`: Rust-based management utility.
- `config.toml`: Master security policy (read-only system roots, whitelisted commands).
- `workspace/`: Persistent node identity, SOPs, and role definitions.
- `prompts/`: MiniJinja templates for autonomous tool discovery.
