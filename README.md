# OpsNanny

![engine: deterministic](https://img.shields.io/badge/engine-deterministic-blue) ![ai: enriching](https://img.shields.io/badge/ai-enriching-blue) [![status: prerelease](https://img.shields.io/badge/status-prerelease-orange)](#state-of-this-project) [![ci](https://github.com/thiscantbeserious/ops-nanny/actions/workflows/ci.yml/badge.svg)](https://github.com/thiscantbeserious/ops-nanny/actions/workflows/ci.yml) 

![Debian: supported](https://img.shields.io/badge/Debian-supported-brightgreen) ![OpenMediaVault: supported](https://img.shields.io/badge/OpenMediaVault-supported-brightgreen) ![TrueNAS: planned](https://img.shields.io/badge/TrueNAS-planned-lightgrey) ![Unraid: planned](https://img.shields.io/badge/Unraid-planned-lightgrey)

![antigravity-cli: supported](https://img.shields.io/badge/antigravity--cli-supported-brightgreen) ![openai-api: planned](https://img.shields.io/badge/openai--api-planned-lightgrey)

<img src="assets/logo-draft.png" alt="OpsNanny mascot: a pixel-art technician in monitoring goggles holding a server rack with a green checkmark and an amber warning icon" width="110" align="right">

### 24/7 AI Server-Supervisor

It watches the systemd journal,
disk health, ZFS pool state, and hardware sensors, turns what it finds into
a plain-language report, and delivers that report to Telegram.

Built on strong isolation principles with a deterministic first approach that doesn't stop when connection to the LLM-Engine gets lost.

It currently never writes to the host it watches. There is no remediation, no
self-healing, no action taken on your behalf, only observation and
notification. If a disk is failing, this tells you.

## Status

**I'm testing this with my real data on my Backup-Server**. To ensure stability this repo tests via real e2e-VM images to ensure we're not breaking any sensitive stuff with updates randomly (will exercise a full install from scratch). This will support interaction soon ("Did anything happen in the last week that needs my attention?"), might do Layer 3 meshing later. Not sure about self-healing or control-layers ... I'm not taking any PR's due to Security Reasons, neither will I randomly support other Chat Integrations (Slack might be supported later).

## 1. Check (read properly)

```bash
curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/ops-nanny/main/install.sh | sudo bash -s -- --dry-run
```

## 2. Install (should be run interactively)

```bash
curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/ops-nanny/main/install.sh | sudo bash
```

## 3. Run (and move away)
```bash
cd /opt/sentinel && docker compose up -d
```

The installer creates the compose stack and installs `rasdaemon` and
`lm-sensors`. Wiring `smartd` and ZED to send mail is asked separately and
defaults to no, and on a host that manages those files itself, such as
OpenMediaVault, it declines rather than asking, because overwriting them
would be reverted by the platform and could displace configuration you rely
on. Re-run it after `docker compose up -d` to seed apprise.

The stack directory is auto-detected: an OpenMediaVault compose root is used
when one is found, `/opt/sentinel` otherwise, and `--stack-dir` overrides
both. Details, including verifying delivery actually works:
[deploy/README.md](deploy/README.md).

## What it watches

- **The systemd journal**, kernel lines at `emerg`/`alert`/`crit` priority,
  `smartd` and ZED log entries, service failures.
- **`smartd`**, SMART health and pending/reallocated sector counts, per disk.
- **ZFS (`zed`)**, pool state, scrub results, checksum/degraded events.
- **`lm-sensors`**, temperatures, voltages, fan speeds.
- **`rasdaemon`**, MCE, ECC, and PCIe AER hardware error events.

Facts are collected deterministically, then handed to an LLM (Antigravity
CLI) for classification and a written recommendation, trend vs. transient,
what it means, what to check next. The LLM never runs commands and has no
access to the host beyond the facts it is given.

## The hardware alerting path does not depend on the LLM

This is the property that separates it from piping logs at a model and
hoping. `smartd` and ZED talk SMTP straight to the notification stack, with
no supervisor process and no LLM in that path at all. And inside the
supervisor itself, a kernel line at `emerg`/`alert`/`crit` is forwarded
immediately, unanalyzed, the moment it's seen, before the LLM step even
runs. If the analyzer is down, unreachable, unauthenticated, or returns
garbage, hardware alerts still arrive; only the triage and recommendation
text is lost. The LLM adds interpretation on top of a path that already
works without it, it is never the thing standing between a failing disk
and a notification.

## Architecture

```mermaid
flowchart LR
    subgraph host["host"]
        journal[("journal")]
        smartd["smartd"]
        zed["zfs-zed"]
        sensors["lm-sensors /\nrasdaemon"]
    end

    subgraph supervisor["sentinel (read-only container)"]
        collect["collect"]
        raw["raw-alert path\n(LLM-free)"]
        analyze["analyze (LLM)"]
        notify["notify"]
        collect --> raw
        collect --> analyze --> notify
    end

    apprise["apprise"]
    mailrise["mailrise"]
    tg(("Telegram"))

    journal -.ro mount.-> collect
    sensors -.ro mount.-> collect
    raw -- POST --> apprise
    notify -- POST --> apprise
    smartd -- SMTP --> mailrise
    zed -- SMTP --> mailrise
    apprise --> tg
    mailrise --> tg
```

`sentinel` runs unprivileged, `read_only: true`, every mount but its own
state directory `:ro`. `apprise` holds the Telegram credential; the
supervisor never does. Full design and the reasoning behind each of these
choices: [ARCHITECTURE.md](ARCHITECTURE.md). Field-level behavior of every
component: [CONTRACTS.md](CONTRACTS.md) and [contracts/](contracts/).

## State of this project

The image builds, passes its test suite (including live checks against
real `apprise`/`mailrise`/`msmtp` containers), and publishes to GHCR for
both `amd64` and `arm64`. It is deployed and running on a real OpenMediaVault
NAS, pulling from GHCR under the production security flags: the analyzer
runs clean tick to tick, notifications reach Telegram, and a deliberately
provoked event was detected, classified, delivered, and its resolution
delivered too, end to end.

That rollout is about a day old, not weeks. No disk has actually failed
under it, so the hardware-alerting path has been exercised only by the
deterministic raw-alert path and by synthetic events, never by a real
failure, and the extended trial that would confirm it behaves correctly
against real hardware over real time has not happened. Treat it
accordingly, consistent with "prerelease" above, until that changes.
