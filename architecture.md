# Architecture: Robust Model-Agnostic Supervisor

## Overview
Transitioning from a Gemini-specific shell-based supervisor to a robust, provider-agnostic system using the **ZeroClaw** framework.

## Core Components

### 1. Agent Engine: ZeroClaw
- **Role:** Handles the LLM interaction loop, tool execution, and provider failover.
- **Language:** Rust (Performance & Safety).
- **Providers:** Gemini, OpenAI, Anthropic (via trait-based abstraction).

### 2. Event Orchestration (Ticks)
- **Mechanism:** ZeroClaw SOPs (Standard Operating Procedures).
- **Trigger:** Cron-based execution (e.g., every 5 minutes).
- **Responsibility:** Running health checks, detecting trends, and escalating alerts.

### 3. Tooling (MCP)
- **Interface:** Model Context Protocol (MCP).
- **Migration Path:**
    - Wrap existing bash probes (`probe-acp.sh`, `probe-raw.sh`) as MCP tools.
    - Implement new tools for provider-agnostic auth and state management.

### 4. Safety & Sandboxing
- **Level:** Supervised (Approval for high-risk commands).
- **Isolation:** Linux Landlock/Bubblewrap for shell command execution.

## Implementation Steps
1. [ ] Install ZeroClaw and verify connectivity with multiple providers.
2. [ ] Define the "Server Sentinel" personality in `zeroclaw.toml`.
3. [ ] Configure SOPs for periodic health monitoring.
4. [ ] Migrate current bash logic into a unified MCP server.
5. [ ] Implement multi-channel alerting (Slack/Webhook).
