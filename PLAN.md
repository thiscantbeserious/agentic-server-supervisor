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

- **T1 — notification stack**: compose (apprise + mailrise) + configs + `.env.example` (full C3 table). **DONE — both paths verified against real Telegram delivery, 2026-08-17.**
  AC: both containers healthy; no secrets in git. V: `curl POST /notify/sentinel` → Telegram; `swaks --to omv@mailrise.xyz --server localhost:8025 --auth-user …` → Telegram.
  Two traps the first bring-up found, both documented in `deploy/README.md`: `mailrise.conf` must be 0644 (uid 999 cannot read 0600, and `restart: unless-stopped` reports `Up` while it crash-loops), and the apprise key must be registered via `POST /add/<key>` — a hand-written `/config/<key>.cfg` is ignored and `notify` then returns **204**, a 2xx that sends nothing.
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
  **Carried in from T4/T5 — two obligations, both must be closed here:**
  1. **`tick` must nil-check `analyze.Run`'s report before marshaling.** On a cancelled context `Run` returns `(nil, err)` and authors nothing (contracts/analyze.md §1), so the SIGTERM path this exception exists to clean up is exactly where a nil-panic would land. The AC above exercises SIGTERM during a tick, so this obligation and its test already coincide — do not let that test pass by never reaching the nil branch.
  2. **`analyze` should emit `findings[].key` inside `resolved[]`.** Today `resolved[]` carries evidence truncated to 80 runes, so the resolve seam is identified by a *different* value from the one every other seam uses — `state` matches it against both headline and evidence to compensate (contracts/state.md S.3(e)). Two findings whose evidence agrees in its first 80 runes collide, and the truncation itself can produce a value matching nothing. Switching to the 16-hex key retires both the collision class and the dual-match workaround. This is a contract amendment to analyze §6 and state S.3(e) together — neither alone.
- **T7 — image + CI + install-host.sh**: Dockerfile (builder + debian-slim), compose `sentinel` service (C4 mounts, group_add, read_only), GHCR workflow (`go vet` + `go test ./...` + build + push latest/SHA), `install-host.sh`, `test/container_test.go`.
  AC: container starts unprivileged; journal reading works via group_add; write attempts on every ro mount fail; empirical checks of the unverified points (ARCHITECTURE §2.6): `sensors -j` returns values, rasdaemon path readable, tmpfs/DNS ok, ZED events under `-t zed`; `sentinel health` drives the compose healthcheck; install-host.sh twice without error; Actions run green, pull from GHCR works. V: `go test -tags container ./test/...` on a Linux host.
### T7 obligation: preflight must verify the journal is readable

`JOURNAL_GID` is discovered once, by `install-host.sh` step 6 (`getent group systemd-journal | cut -d: -f3`), and compose refuses to start without it. That is sound at install time. **Nothing re-verifies it at runtime**, and R2's preflight does not read the journal at all — it checks config, the filesystem, `/usr/local/bin` writability, agy seeding and the state directories.

So a stale or wrong value — `.env` copied to another host, the gid changed by a distro upgrade, `group_add` silently ineffective — produces a container that starts cleanly, reads an empty journal, and reports all-clear indefinitely. An empty journal is indistinguishable from a quiet system, which makes this the quietest possible failure of a component whose entire job is noticing things.

**T7 adds to R2's startup sequence**, before the first tick:

```sh
journalctl -D $HOST_JOURNAL_DIR -n1 --no-pager -o json     # and/or the volatile dir
```

Zero bytes of stdout ⇒ log `ERROR` naming the likely cause (`JOURNAL_GID`, `group_add`, or the `/host/journal` mount) and exit `78`, consistent with every other config failure. A readable journal on any running host yields at least one JSON record.

**Two details, both measured in `debian:trixie-slim` + `systemd` on 2026-08-19 — the exact R1 runtime base — and both of which make the obvious implementations vacuous:**

1. **The exit code proves nothing, and neither does non-empty stdout.** With no journal present at all — bare, and with `-D <empty dir>` — `journalctl -n1 --no-pager` **exits 0** and prints `-- No entries --` to stdout. A check on the exit code passes on a blind container; so does a check that stdout is non-empty. **`-o json` is what discriminates**: 0 bytes when there is nothing to read, a JSON record when there is. Assert on the byte count of the JSON form, never on the exit code and never on the human-readable output.
2. **`-D` is required.** An earlier wording said "run `journalctl -n1`, unfiltered". Taken literally that reads `/var/log/journal` and `/run/log/journal`, which R4 does not mount at those paths — preflight would fail on every host and the container would never start. "Unfiltered" means no `-p`, `--since` or `-u`; it does not mean no `-D`.

**One readable directory of the two is sufficient**, consistent with R2 step 2's existing mount check, which already accepts at least one of persistent/volatile.

R8 case C2 already asserts the build-time half; this extends it to the running host, where the value actually drifts. Where no host journal exists — a developer Mac — the check's test must **skip loudly**, never pass.

R8 case C2 already asserts `journalctl -D /host/journal -n1` exits `0` with the gid and non-zero without it, so the check is verified at build time; this extends the same assertion to the running host, which is where the value can drift.

Deliberately not in T6: `internal/runtime` was reviewed and approved without it, and reopening an approved branch to add behaviour is how an approval stops meaning anything. T7 re-verifies the runtime inside the image and owns C2, so it is the natural home.

### Read-only survey of `bam`, 2026-08-18

Measured before T7 so the image is written against the host as it is, not as assumed. Read-only throughout; nothing installed, written or restarted.

| Assumption | Measured |
|---|---|
| Debian 13 | trixie, systemd 257, kernel 7.1.3+deb13-amd64 |
| **`JOURNAL_GID=999`** | **holds** — `systemd-journal:x:999` |
| smartmontools | installed (`/usr/sbin/smartd`, `/usr/sbin/smartctl`) |
| lm-sensors | **not installed** — `install-host.sh` installs it |
| rasdaemon | **not installed** — `install-host.sh` installs it |
| msmtp | **not installed** — `install-host.sh` installs it |
| ZFS pools | `cache` 928G and `hotstore` 16.4T, both `ONLINE` |
| `zfs-zed` | **active** — ZFS events already reach the journal |
| docker compose | v5.5.0 |

The gid is the one that had to hold. A wrong `JOURNAL_GID` gives the unprivileged container an empty journal, and an empty journal reads as healthy — the quietest failure this system has.

**`smartd` is running but monitoring zero devices.** Verbatim from the host journal:

```
smartd[2658]: Configuration file /etc/smartd.conf parsed but has no entries
smartd[2658]: Monitoring 0 ATA/SATA, 0 SCSI/SAS and 0 NVMe devices
```

So `bam` has no SMART monitoring today, and `internal/collect` sources the `smart` section from `journal -t smartd` — it would be permanently empty, which reads as healthy rather than as broken. R3's `install-host.sh` writes the `DEVICESCAN` line that fixes this, which makes that script the difference between disk monitoring existing and not. T8 must confirm after running it that `smartd` reports a non-zero device count, not merely that the unit is active.

**Care with `command -v` on this host:** the login PATH omits `/usr/sbin`, so `smartd`, `smartctl` and `rasdaemon` all appear absent when they are not. Check with `dpkg-query` or an absolute path. This produced a wrong reading during the survey itself.

- **T8 — target server rollout `bam`** (= OMV host): packages, smartd/ZED mail paths, `docker compose pull` from GHCR, 24 h trial run. Does NOT run autonomously — every action on the host is announced first.
  AC: tick loop runs; heartbeat arrives; injected error ⇒ Telegram < 6 min; `smartctl -M test` mail ⇒ Telegram; `zpool scrub` ⇒ ZED event in the next report; no spam. V: `test/rollout-checklist.md`.
  **Credential hygiene carried from T1 — do before rollout, not during:**
  1. **Rotate the bot token.** The token used for T1's local verification was pasted into a chat transcript, so treat it as compromised: `/revoke` in @BotFather, then update `mailrise.conf` (both the `sentinel:` and `omv:` blocks) and re-run the apprise `POST /add/sentinel`.
  2. **Set a real `MAILRISE_SMTP_PASS`.** Local verification ran on a throwaway. It must match in **both** `.env` and `mailrise.conf` — they are read by different processes and neither warns when they disagree.
  3. **Delete any `/config/<key>.cfg` in the apprise volume.** apprise ignores it while it looks authoritative; the local volume still holds one from T1 diagnosis.
  **Carried from T7 — watch during the trial, do not pre-fix:** `postApprise` shares Go's default HTTP transport pool across calls. Measured against a real apprise-api, a request landing on a connection the server has started tearing down fails with `transport: EOF` at roughly 3/40 (7.5%). Steady-state ticks are minutes apart against `IdleConnTimeout`'s 90s, so this should not surface in normal operation — the one path that posts back-to-back is `drainOutbox` draining a multi-item backlog, which only accumulates after apprise has already been down. If it recovers and the drain trips an EOF on a queued item, that item stays queued and retries next tick, having burned one attempt and moved SMTP escalation one step closer — degraded, not lost. Check the trial logs for `transport: EOF` during any post-recovery drain; if it recurs, `DisableKeepAlives` on the notify client is the proven one-line fix, held out of T7 as a deliberate scope call.
- **T10 (optional, later) — mute**: suppress notifications for a chosen window without stopping the supervisor.
  **Build after T8's 24h trial, not before.** The trial is the data that says whether mute is needed, which durations matter, and what should bypass one. If the trial yields four messages a day the answer is "not needed"; if it yields forty, the interesting question is why, and a mute button would be treating the symptom.

  **Three designs, in cost order:**
  1. **Host CLI (recommended start).** `sentinel mute 6h` writes `$STATE_DIR/mute-until` as RFC3339; `state` reads it and suppresses notification while active; `sentinel mute --status` and `sentinel unmute` round it out. No token, no inbound channel, roughly one state file and a check in `Process`.
  2. **Telegram command — has a security cost that must be stated.** Accepting `/mute 6h` from Telegram means `sentinel` polls `getUpdates`, which means giving the bot token to the process that parses attacker-controlled log text. That is the one boundary the whole notification design is built around (ARCHITECTURE §3: `notify` is the only component that knows what the notification service is, and it holds no credential). **Do not take this option.**
  3. **Separate poller container.** A fourth service in the compose stack holds the token, polls `getUpdates`, and writes the same `mute-until` file to the shared volume. `sentinel` stays token-free and only reads the file. Gets phone-side control while keeping the boundary; costs one more container. Option 1 is a prerequisite either way, since both write the same file.

  **The open design question, which the trial should answer:** what bypasses a mute? A mute that hides a dying disk is how people lose pools. The instinct is that `alert` severity always gets through while `watch`/`info` and the heartbeat are suppressed, with the muted state named in the message so the operator knows it is overriding — but that is a guess about how noisy `bam` actually is, and the trial replaces the guess.

  **Whatever the rule, the exit code and `sentinel health` must never be muted** — same principle as the raw-alert scan-failure throttle (runtime R3.3): quiet in the channel a human reads, loud everywhere a machine looks.

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
