# deploy/

The notification stack and the supervisor service.
`docker-compose.yml` is the single stack: `apprise` and `mailrise` deliver
notifications; `sentinel` is the supervisor itself, defined exactly as
`contracts/runtime.md` specifies so the two cannot drift.

## Why two services

`apprise` is the one component that knows how to reach Telegram. `sentinel`
POSTs JSON to it and never holds the bot token — the token stays out of the
process that parses attacker-controlled log text.

`mailrise` exists because the things most likely to notice a dying disk can only
send mail: OpenMediaVault, `smartd -m`, and `zfs-zed`'s `ZED_EMAIL_ADDR`. They
deliver SMTP to mailrise, which forwards through the same apprise instance. That
is the **LLM-free path** — it works when the supervisor is down, when agy is
unreachable, and when the analyzer is falling back.

## Quick install

The normal way in — nothing copied onto the host first:

```bash
curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/agentic-server-supervisor/main/deploy/install-host.sh | sudo bash
```

This installs the host packages (rasdaemon, msmtp, smartd/ZED wiring — see
`contracts/runtime.md` R5) **and** creates the compose stack itself: it
detects whether the host is running OpenMediaVault's compose plugin and
lays the stack out accordingly (`/docker-compose/sentinel/`, OMV's
symlink shape, or a plain `/opt/sentinel/` otherwise), offering the
detected directory in a prompt — press Enter to accept it, or type a
different path. It then prompts for the Telegram bot token, chat id, and
a mailrise SMTP password, writes them into the stack's env file and into
`mailrise.conf` (never echoed, never logged), and fetches
`docker-compose.yml` from this same repository. Re-running is safe and
idempotent: it only fills in what is still missing.

Pin a specific version instead of `main` with `--ref`:

```bash
curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/agentic-server-supervisor/main/deploy/install-host.sh | sudo bash -s -- --ref v1.2.3
```

`--check` and `--dry-run` never prompt and never write — safe to run
repeatedly to preview what would happen, including where the stack would
be created and which layout it would choose. Full flag reference:
`install-host.sh --help`, or `contracts/runtime.md` R5.

**Read the script first if you want to** — a `curl | sudo bash` that
installs packages and writes to `/etc` is a reasonable thing to want to
read before running as root. It is a plain, gitignore-respecting bash
script; nothing about it requires the pipe:

```bash
curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/agentic-server-supervisor/main/deploy/install-host.sh -o install-host.sh
less install-host.sh
sudo bash install-host.sh --ref main   # same behavior, run from disk
```

## Setup (manual — filling in `.env` by hand instead of being prompted)

```bash
cd deploy

cp .env.example .env
chmod 600 .env
$EDITOR .env                      # TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID, MAILRISE_SMTP_*

cp mailrise/mailrise.conf.example mailrise/mailrise.conf
chmod 644 mailrise/mailrise.conf  # NOT 600 — see below
$EDITOR mailrise/mailrise.conf    # same token and chat id, same SMTP user/pass

docker compose up -d
docker compose ps                 # apprise must reach (healthy)
```

Both `.env` and `mailrise/mailrise.conf` are gitignored. The `.example` files
are tracked and must never contain a real token.

**`mailrise.conf` must be world-readable (0644), not 0600.** mailrise runs as
uid 999 inside its container and cannot read a file owned by your host user
with 0600 — it exits immediately with `Permission denied: '/etc/mailrise.conf'`.
`restart: unless-stopped` then hides it: the container crash-loops while
`docker compose ps` keeps reporting `Up`, and the SMTP port simply never
answers. Verified locally 2026-08-17.

The tradeoff is real — the file holds the bot token, so 0644 exposes it to any
local user on the host. Do not chown it to 999 instead: on `bam` gid 999 is
`systemd-journal`, which would hand the token to every journal reader.

**mailrise does not expand environment variables** — it loads the file as plain
YAML, so a literal `$TELEGRAM_BOT_TOKEN` would be accepted as part of the URL
and produce an invalid Telegram target. You would discover that the first time
an alert failed to arrive, which is why the values are written in rather than
interpolated.

## Deploying under OpenMediaVault

**The Quick install above already does everything in this section for
you** — it detects an OMV compose root and lays the stack out correctly,
including every constraint below. This section is for anyone using the
manual `--env-file` path against a stack directory they built by hand
instead, where none of these constraints are enforced automatically.

OpenMediaVault's compose plugin owns `/docker-compose/`, one directory per
stack, and does not lay the stack out the way the plain `cd deploy` Setup
above assumes:

```
/docker-compose/<stack>/
├── <stack>.yml                 real compose file, named after the stack
├── compose.yml  -> <stack>.yml     symlink
├── <stack>.env                 real env file
├── .env         -> <stack>.env     symlink
└── compose.override.yml        optional
```

There is also a shared `/docker-compose/global.env` the plugin injects into
every stack. It is currently empty; anything added there later applies to
this stack too.

The Setup steps above still apply — `cp`, `chmod`, edit, `docker compose
up -d` — with three OMV-specific constraints:

**`--env-file` must target the real env file, never the `.env` symlink.**
`install-host.sh` writes `JOURNAL_GID=` via `mktemp` then `install`, and
`install` replaces its destination rather than following it. Pointed at
`.env`, it would silently turn the symlink into a regular file, leaving a
stale `<stack>.env` beside a now-divorced `.env` — the OMV plugin and Docker
then disagree about what the environment is, and nothing announces it. Use
`--env-file /docker-compose/<stack>/<stack>.env`, the real file, always.

**`mailrise/mailrise.conf` is a relative bind mount** (`docker-compose.yml`:
`./mailrise/mailrise.conf:/etc/mailrise.conf:ro`), so a `mailrise/`
subdirectory has to exist inside the stack directory itself — everything
else in the compose file is either a named volume or an absolute path, and
this mount is the only one that moves with the stack. The 0644-not-0600
rule above applies to that copy: get the permission wrong here, standing in
the actual stack directory, and mailrise crash-loops exactly as described.

**`AGY_CREDENTIALS_DIR` pointing at a path that does not exist fails
silently.** Compose only guards that the variable is *set*
(`${AGY_CREDENTIALS_DIR:?...}`), not that the path exists, and Docker
creates a missing bind source as an empty root-owned directory. `agy` then
starts with no credentials and never authenticates — while `docker compose
ps` reports everything running. Confirm the directory exists and holds real
credentials before `up -d`.

The degraded behavior in that state is bounded, not total: every triage
call fails and `internal/analyze` returns its deterministic fallback report
— one `alert`-severity finding (`component: meta`) per tick, carrying all
of that tick's raw `emerg`/`crit` kernel lines (capped at
`RAW_ALERT_MAX_LINES`, default 20) verbatim in its evidence, not one
finding per line. No hardware event is lost; only the LLM's own triage,
correlation and recommendation text is.

## Seed the apprise config key

`apprise/sentinel.cfg` is a template. The real configuration lives in the
`apprise-config` volume and carries the token. **Register it through the API —
do not drop a file into `/config`:**

```bash
curl -fsS -X POST -d 'urls=tgram://<BOT_TOKEN>/<CHAT_ID>' \
  http://127.0.0.1:8000/add/sentinel
# -> Successfully saved configuration
```

Writing `/config/sentinel.cfg` directly does **not** register the key. This
image keeps configurations under `/config/store/<key>/`, so a hand-written
`<key>.cfg` is ignored — and the failure is silent in the worst way: a
subsequent `POST /notify/sentinel` returns **HTTP 204**, which looks like
success, while nothing is sent anywhere. A correctly registered key returns 200
on delivery, or 424 with the reason when delivery fails. Verified locally
2026-08-17.

`sentinel notify --seed-config` performs the same registration; it is an ops
one-shot the runtime never invokes.

## Verify

Two paths reach Telegram: the apprise path sentinel uses directly, and the
SMTP path smartd/ZED/OMV use through mailrise. Both must land. Until they do
the stack is not working — the supervisor can compute a perfect report and
still never reach you.

A 2xx is not proof on its own: **204 means the key was never registered and
nothing was sent.** Success is 200 plus the message actually arriving.

```bash
# 1. The path sentinel uses.
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"title":"sentinel test","body":"apprise path works","type":"info"}' \
  http://127.0.0.1:8000/notify/sentinel

# 2. The path smartd, ZED and OMV use.
swaks --to omv@mailrise.xyz --server 127.0.0.1:8025 \
  --auth-user "$MAILRISE_SMTP_USER" --auth-password "$MAILRISE_SMTP_PASS" \
  --header 'Subject: smartd test' --body 'mailrise path works'
```

## Local development on macOS (podman)

The compose shim cannot reach podman over the `ssh://` connections
`podman system connection list` advertises. Point it at the machine's unix
socket instead:

```bash
export DOCKER_HOST="unix://$(podman machine inspect podman-machine-default --format '{{.ConnectionInfo.PodmanSocket.Path}}')"
```

Not needed on `bam`, which runs real Docker.

## Binding

`APPRISE_BIND` and `MAILRISE_BIND` default to `127.0.0.1`. Widen to the host's
LAN address only when other machines must deliver mail here, and never to
`0.0.0.0`: anyone who can reach the apprise port can send messages as your bot.

## Operational note

Restarting this stack does not lose alerts. When apprise is unreachable the
supervisor queues to its outbox and retries; after `OUTBOX_SMTP_AFTER` failed
ticks it switches to the mailrise SMTP path for the same payload.
