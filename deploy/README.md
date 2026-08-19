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

## Setup

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

Both must land in Telegram. Until they do the stack is not working — the
supervisor can compute a perfect report and still never reach you.

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
