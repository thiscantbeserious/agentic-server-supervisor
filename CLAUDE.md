# Working Rules for Claude Code

Architecture: [ARCHITECTURE.md](ARCHITECTURE.md) · Technical plan with TODOs: [PLAN.md](PLAN.md) · Field-level contracts (authoritative): [CONTRACTS.md](CONTRACTS.md) · Execution protocol (TDD + gates): PLAN.md §4

## Ground Rules
- **No TODO starts without the user's explicit go.** Feedback is not a go.
- **Read-only towards the target server `bam`** (doh@192.168.1.151, key `~/.ssh/sentinel_ed25519`): strictly read commands only; every action there is announced first. Installs/deploys only in T8 after approval.
- Local development/tests: Podman (`docker` shim available). Secrets only in `.env` (gitignored), never in code, commits, or logs.
- All repo documents, comments, and commit messages are written in **English**. Diagrams are **Mermaid**, never ASCII art.
- One commit per completed TODO: `T<n>: <deliverable>`.
- TDD first — RED → GREEN → REFACTOR, then gates, then review. Full protocol: PLAN.md §4.

## Model Matrix (fixed)

| Role | Model | Rationale |
|---|---|---|
| Orchestration + independent verification | session model (main agent) | holds overall context |
| Implementer — mechanical TODOs: **T5** | `haiku` | LLM-free state logic against the CONTRACTS.md test table + SENTINEL_NOW clock — fully specified |
| Implementer — foundation & runtime TODOs: **T1, T2, T3, T4, T6, T7** | `sonnet` | T2 sets the conventions every later package imports; container/agy/E2E work needs judgment |
| Reviewer (every TODO) | `opus` | adversarial review is the quality anchor |
| Runnability second opinion | `agy` (Gemini) | deliberately different vendor — independent third view |

If a Haiku implementer fails the gate or review twice in a row, escalate that TODO to `sonnet` (spec gap — don't grind).

## Agent Communication (Agent Teams)

With `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` active, the per-TODO loop runs as a **team**: implementer and reviewer are spawned as named teammates and talk to each other directly via `SendMessage` — the REJECT defect list, follow-up questions about a contract clause, and the re-review request go peer-to-peer instead of through main-agent respawns. Rules:

- The main agent remains the **gate authority**: independent V run, container smoke run, and agy second opinion stay with the main agent; teammates cannot approve their own gate.
- The reviewer's fresh-context property must survive messaging: the implementer sends the diff + RED/GREEN outputs, never its own reasoning transcript.
- Iteration cap (3) still counts per REJECT→fix round, whoever transports the message.
- If the teams flag is not active in the session, fall back to the classic loop (main agent relays between fresh spawns) — same protocol, more context cost.

## Reporting Behavior (applies to me, not just the supervisor)
Classify findings first (transient vs. trend, redundancy state), then report with a concrete, conditional recommendation — no blind warnings (A9, ARCHITECTURE §1).
