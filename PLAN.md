# Technical Implementation Plan: Agentic Server Supervisor (v4, Go)

**Architecture & rationale:** [ARCHITECTURE.md](ARCHITECTURE.md) · **Field-level contracts (authoritative):** [CONTRACTS.md](CONTRACTS.md) · **Working protocol/gates:** [CLAUDE.md](CLAUDE.md)
**Date:** 2026-08-15

## 0. Language Decision (v4)

The supervisor is **one Go binary `sentinel`** with subcommands `collect | analyze | state | notify | tick | health` — not bash. Decided after independent research (adversarially sourced, see ARCHITECTURE §2 addendum):

- The real logic (state-machine dedup, rate-limit time math, byte-budget truncation, JSON assembly from 9 sources) sits exactly where bash is weakest; typed structs + table-driven `go test` are far more reliable for TDD by LLM implementers.
- No libsystemd/sdjournal bindings: `CGO_ENABLED=0`, journal access execs `journalctl -o json` as a subprocess (Vector precedent); `sensors -j` and `agy` are exec'd subprocesses too. The image therefore needs systemd tools + lm-sensors + agy regardless of language — the win is dropping bash/jq/curl/coreutils and gaining types + tests.
- Rust rejected: no perf/concurrency need at 1 tick / 5 min; CI cost; project history.

## 1. Repo Layout (target)

Two clean halves: the repo root is a **standard Go project** (plus the four docs); everything that ships to or runs on the server lives under **`deploy/`**.

```
├── ARCHITECTURE.md                 # what & why
├── PLAN.md                         # this document: how (TODOs, protocol)
├── CONTRACTS.md                    # binding conventions C1–C9 + index
├── contracts/                      # per-component contracts, one file per implementation stage
├── CLAUDE.md                       # working rules: TDD loop + gates + model matrix
│
├── go.mod                          # github.com/thiscantbeserious/agentic-server-supervisor
├── cmd/sentinel/main.go            # dispatch + exit-code mapping (the only os.Exit)
├── internal/                       # one package per concern, single owners (CONTRACTS §C1)
│   ├── config/  logging/  facts/  report/  dedup/
│   ├── journal/ collect/  analyze/ state/  notify/  runtime/
├── test/container_test.go          # build tag `container` (T7 smoke assertions)
│
├── deploy/                         # everything ops — this directory goes to the server
│   ├── docker-compose.yml          # ONE stack: sentinel + apprise-api + mailrise
│   ├── .env.example                # every env var from CONTRACTS.md §C3
│   ├── Dockerfile                  # golang builder stage → debian-slim runtime (journalctl, lm-sensors, agy)
│   ├── apprise/sentinel.cfg        # seeded via `sentinel notify --seed-config` (ops one-shot)
│   ├── mailrise/mailrise.conf      # SMTP ingest → tgram://
│   └── install-host.sh             # host part: rasdaemon, smartd→mailrise, zed→mailrise, JOURNAL_GID → .env
│
└── .github/workflows/build.yml     # go vet + go test + build + push → ghcr.io (latest + SHA)
```

Rollout on `bam` is exactly: copy `deploy/`, fill `.env`, run `install-host.sh`, `docker compose up -d`. Embedded assets (schemas, prompt) live inside their `internal/` packages via `go:embed` — no loose config files to ship.

## 2. Component Specification

**[CONTRACTS.md](CONTRACTS.md) is authoritative** — CLI, env vars (C3), paths/mounts (C4), shared types & JSON rules (C5), the dedup algorithm (C6), logging (C7), in-process seams (C8), test layout (C9), plus per-component contracts under [contracts/](contracts/) (split per implementation stage; each implementer reads C1–C9 plus only its stage's file) with struct definitions, example documents, exit codes, failure behavior, and 1:1 test tables. Implementers make zero design decisions.

Summary of what stays true from v3 (unchanged semantics, now typed):

- **collect** — 9 sections (kernel, ras, smart, sensors, zfs, resources, services, network, meta) from `:ro` mounts; per-section timeout & failure isolation; deterministic 256 KB truncation; `--deep <zfs|smart|kernel|ras>` for the deep dive. No zpool/smartctl in the container.
- **analyze** — agy exec with embedded prompt + last N=5 reports; output ALWAYS schema-validated in-process, 1 retry, deterministic fallback report; triage + deep dive (A9): new finding ⇒ deep collect ⇒ second agy call ⇒ `analysis` + conditional `recommendation`; max 1 deep dive per tick, rest queued in `deep-queue/`.
- **state** — dedup via `dedup.Key` (one algorithm, one package); re-notify windows ALERT 1h / WATCH 6h; resolved ⇒ exactly one all-clear; history rotation 50; outbox with SMTP fallback after 3 failed ticks; daily 08:00 heartbeat; `heartbeat` file mtime doubles as liveness for `sentinel health`.
- **notify** — apprise JSON POST (`[STATUS] host: headline`), body sanitization, outbox handoff on failure. Telegram credentials never enter the sentinel container.
- **runtime** — sequential tick: collect → **raw alert for emerg/crit before the LLM step (LLM-free)** → analyze → state → notify; `--loop` with SIGTERM-clean shutdown; atomic writes (`CreateTemp` → `Sync` → `Rename`) everywhere.
- **Container security model unchanged:** `read_only: true`, `cap_drop: ALL`, `no-new-privileges`, unprivileged user, `group_add: ${JOURNAL_GID}`, mounts per CONTRACTS §C4, rw only `/state` volume + `/tmp` tmpfs.

## 3. TODOs (implementation order)

Every TODO has **deliverable, acceptance criteria (AC), verification (V)**. Done = V green **+ runnability gate passed + reviewer APPROVE** (protocol §4). All code per CONTRACTS.md — deviations require a contract change first, not silent drift.

- **T1 — notification stack**: compose (apprise + mailrise) + configs + `.env.example` (full C3 table).
  AC: both containers healthy; no secrets in git. V: `curl POST /notify/sentinel` → Telegram; `swaks --to omv@mailrise.xyz --server localhost:8025 --auth-user …` → Telegram.
- **T2 — shared foundation**: `go.mod`, `cmd/sentinel` skeleton (dispatch, exit-code map, `--version`), `internal/config`, `internal/logging`, `internal/facts`, `internal/report`, `internal/dedup` with embedded schemas.
  AC: config validation exits 78 naming the variable; `report.Validate` rejects the negative fixtures; `dedup.Key` matches the C6 vectors incl. the ZFS CKSUM case. V: `go test ./internal/...` (foundation packages).
- **T3 — collect**: `internal/journal` + `internal/collect` per contract (sections, timeouts, isolation, truncation, `--deep`).
  AC: valid against facts schema; injected `logger -p kern.err "SENTINEL-TEST"` appears in `kernel`; a stubbed-away section binary yields an `error` field, not an abort; truncation is deterministic at the byte budget. V: `go test ./internal/collect/... ./internal/journal/...` + in-container run.
- **T4 — analyze**: `internal/analyze` per contract (agy exec, history, validation + retry + fallback, triage + deep dive, deep-queue).
  AC: clean facts ⇒ OK; kern.err fixture ⇒ ≥ WATCH with a human-readable explanation; ZFS CKSUM fixture ⇒ WATCH with `analysis` + `recommendation` (not ALERT); agy stub missing ⇒ fallback ALERT (exit 3 path); agy stub emitting broken JSON ⇒ retry then fallback. V: `go test ./internal/analyze/...` (agy stubbed via `AGY_BIN`), plus one real agy call locally.
- **T5 — state**: `internal/state` per contract (Process, history, outbox, heartbeat).
  AC: 3 ticks same finding ⇒ exactly 1 notification + 1 escalation; resolved ⇒ exactly 1 all-clear; outbox SMTP fallback after 3; clock driven via `SENTINEL_NOW`. V: `go test ./internal/state/...`.
- **T6 — notify + runtime (E2E)**: `internal/notify` + `internal/runtime` per contract, wired through `sentinel tick --once`.
  AC: full tick against the real T1 stack; kern.crit fixture produces the raw alert with agy stubbed away; SIGTERM during a tick finishes the tick then exits 0. V: `go test ./internal/notify/... ./internal/runtime/...` + `test/test-e2e.sh` → Telegram message `[STATUS] host: headline`.
- **T7 — image + CI + install-host.sh**: Dockerfile (builder + debian-slim), compose `sentinel` service (C4 mounts, group_add, read_only), GHCR workflow (`go vet` + `go test ./...` + build + push latest/SHA), `install-host.sh`, `test/container_test.go`.
  AC: container starts unprivileged; journal reading works via group_add; write attempts on every ro mount fail; empirical checks of the unverified points (ARCHITECTURE §2.6): `sensors -j` returns values, rasdaemon path readable, tmpfs/DNS ok, ZED events under `-t zed`; `sentinel health` drives the compose healthcheck; install-host.sh twice without error; Actions run green, pull from GHCR works. V: `go test -tags container ./test/...` on a Linux host.
- **T8 — target server rollout `bam`** (= OMV host): packages, smartd/ZED mail paths, `docker compose pull` from GHCR, 24 h trial run. Does NOT run autonomously — every action on the host is announced first.
  AC: tick loop runs; heartbeat arrives; injected error ⇒ Telegram < 6 min; `smartctl -M test` mail ⇒ Telegram; `zpool scrub` ⇒ ZED event in the next report; no spam. V: `test/rollout-checklist.md`.
- **T9 (optional, later)** — ZeroClaw investigator (upstream) reacting to ALERT, read-only whitelist. Own branch, only after 2 weeks of stable operation.

**Dependencies:** T1‖T2 → T3 → T4 → T5 → T6 → T7 → T8. (T3–T6 need T2; T6 needs T1.)

**Open inputs from the user:** Telegram bot token + chat ID (for T1 verification); go per TODO.

## 4. Execution Protocol per TODO (TDD first: RED → GREEN → REFACTOR, then gates)

Model assignment per role: see the model matrix in [CLAUDE.md](CLAUDE.md).

**Phase 1 — TDD cycle (implementer subagent):**
1. **RED:** Write the tests FIRST — the table-driven `go test` cases listed in the component's CONTRACTS.md test table, including fixtures. Run them and show they fail for the right reason (missing implementation, not broken tests). No implementation code before a failing test exists.
2. **GREEN:** Write the minimal implementation that makes the tests pass. Run, show real output.
3. **REFACTOR:** Clean up (`go vet`, naming, duplication) while tests stay green. Re-run, show output.

The implementer's report is: the diff + verbatim RED output + verbatim GREEN output. A test that never was red proves nothing and fails the gate below.

**Phase 2 — Gates (main agent):**
4. **Independent verification:** the main agent re-runs V itself. Red ⇒ back to phase 1 with the error output.
5. **Runnability gate (mandatory after every step):**
   a. **Container smoke run — required, not "if convenient":** from T3 onward the affected code is executed **inside a Linux container** (`podman build` + `podman run` with the real security flags `--read-only --cap-drop=ALL --security-opt no-new-privileges`, unprivileged user, tmpfs `/tmp`, named volume `/state`); before T3, via the compiled binary locally. Green tests on the developer machine prove nothing about the target: macOS has no `/proc` and no journal, so on a Mac the `resources` and `network` parsers never execute at all. The gate is not passed until the paste-able evidence exists: the command, the exit code, and the emitted document. Skipping it silently is a protocol violation — T3 shipped its first commit without it and the omission had to be corrected afterwards.
   Minimum assertions: exit code as contracted; the document parses and validates; every `:ro` surface rejects a write while `/state` and `/tmp` accept one; `id` shows the unprivileged uid and `CapEff` is `0000000000000000`; the component wrote **nothing** outside `/state`.
   b. **agy second validation:** an independent second opinion from a different vendor on two questions: "Is this code runnable in the target setup (read_only, cap_drop ALL, ro mounts, unprivileged, CGO_ENABLED=0, debian-slim)? Which runtime errors are foreseeable?" and "Where does the code contradict CONTRACTS.md C1–C9, or hide a bug the tests would miss?" Substantiated findings ⇒ back to phase 1. Invocation (verified 2026-08-16):

   ```bash
   agy -p "<prompt>" --print-timeout 10m
   ```

   Run it **from the repo root and let agy read the files itself** — name the packages and tell it to read CONTRACTS.md first; never inline file contents. Reading the real tree is what makes the gate worth running: it is how the frozen-clock defect was found, by comparing `config.go` against C3 without being handed either.

   Mechanics: print mode does **not** read stdin, so the prompt must be an argument, and `--print-timeout` needs a duration unit (`10m`, not `300`). Permissions live in **`~/.gemini/antigravity-cli/settings.json`** (the path `~/.gemini/settings.json` is silently ignored — the log line `applyUserSettings: no shared config permissions` is the tell), as `action(target)` rules under `permissions.allow`: `read_file(...)` **and** `command(...)` are both needed, because agy shells out to `find`/`grep` as well as using its native reader. Writes and network are in `deny`. Diagnose any denial with `--log-file <path>` and grep `permission_manager` for the exact action it wanted. `--dangerously-skip-permissions` is not used.

   c. **Live validation against reality (mandatory from T3 on, read-only):** synthetic fixtures encode the assumptions of whoever wrote them, so before a TODO can be proposed for merge its assumptions are checked against the actual target `bam` — **strictly read-only, every command announced first** (CLAUDE.md ground rule; installs and deploys remain T8-after-approval). What this means concretely: real data replayed through the new code (e.g. captured `journalctl -o json` records fed through the parser), and every environmental assumption the component makes verified on the host rather than assumed — permissions and GIDs, which paths exist, whether the required binaries are installed, real volumes and record counts.
   This gate has already paid for itself: it confirmed `systemd-journal` GID 999 and that `/run/log/journal` exists (so the two-directory merge is live, not theoretical), showed `/proc/spl/kstat/zfs/` mixes files with pool directories, established that a 24h kernel window is ~1400 records (right-sizing the 20000 cap), and proved the dedup key collapses **real** zed evidence across differing `eid=`/offset/size values.
   Where a component cannot be exercised against the host without deploying (that is T8), say so explicitly in the PR rather than implying coverage that does not exist.

**Phase 3 — Review (reviewer subagent, fresh context):**
6. Sees ARCHITECTURE.md, CONTRACTS.md, the diff, RED/GREEN outputs, and gate outputs — not the implementer transcript. Checks adversarially:
   - Contract honored exactly (types, exit codes, env vars, paths)? Do the tests match the contract's test table? Was the test genuinely red first?
   - **Read-only violated?** (any write outside `/state` and `/tmp` = fail)
   - Secrets in git/code? Prompt-injection surface (analyze: log data marked as data)?
   - Failure paths from ARCHITECTURE §5 covered?
   - Over-engineering: anything not in the TODO ⇒ remove.
   Output: `APPROVE` or `REJECT` with concrete, referenced defects.
7. **Loop:** REJECT ⇒ the reviewer sends the defect list **directly to the implementer** (Agent Teams peer messaging, see CLAUDE.md "Agent Communication"); the implementer fixes, re-runs RED/GREEN evidence, and messages the reviewer back for re-review. Max 3 iterations, then escalate to the user instead of looping. Without the teams flag, the main agent relays the same messages between fresh spawns.
**Phase 4 — Merge request and the user's final review (the last gate is human):**

8. **Commit** `T<n>: <deliverable>` on the TODO's own branch `t<n>-<slug>`. Never on `main`.
9. **Open the PR** (`gh pr create --base main`). The body is the **gate record**, and it is written so the user can audit the work without re-deriving it:
   - what the TODO delivers, and every contract amendment it forced (with the reasoning, since amendments are the least-reviewed artifact in this loop — three of T3's last four defects originated in contract text, not implementation);
   - the reviewer's verdict and how many REJECT rounds it took;
   - agy's findings and what was done with each — including any dismissed, with the reason;
   - **the live evidence**: the container smoke run (command + exit code + result) and the read-only host validation, quoted as output rather than summarized as a claim;
   - what was **not** covered and why (skipped tests, deferred ceilings, anything only T8 can exercise). A gap named in the PR is honest; a gap discovered in the diff is not.
10. **CodeRabbit review loop — a fourth independent gate, run on the PR before the user reads it.** Comment `@coderabbitai full review` on the freshly opened PR, wait for its findings, and treat them like any other gate: triage each one, route real defects to the implementer, fix, push, then comment `@coderabbitai review` to re-run against the new head. Repeat until it reports no actionable findings.

    Triage rather than obey: CodeRabbit does not know this project's contracts, so a finding that contradicts `CONTRACTS.md` is a false positive to be answered in the PR thread, not implemented — and if it turns out the contract is what is wrong, that is a contract amendment and my decision, exactly as with the other gates. Record dismissals with the reason in the thread; a silent dismissal is indistinguishable from an oversight.

    This gate exists for the same reason the agy second opinion does: it reads the diff with no memory of the arguments that produced it. In T4 and T5 the defects that survived adversarial review were the ones where the reviewer's own construction hid them.

11. **The user performs the final review and merges.** No TODO reaches `main` without their explicit approval on the PR — not on an agent APPROVE, not on green gates, not on my own verification. `--no-ff`, so `main` carries one merge commit per TODO. If they request changes, it returns to phase 1 on the same branch; the iteration cap does not apply to their rounds.
12. Task → completed only after the merge. Then wait for their go for the next TODO — a merged PR is not a go for the next one.
