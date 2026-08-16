# deploy/

The notification stack (T1) and, from T7, the supervisor service itself.
`docker-compose.yml` is the single stack: `apprise` and `mailrise` are defined
here; `sentinel` is added in T7 exactly as `contracts/runtime.md` §R3 specifies.

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
chmod 600 mailrise/mailrise.conf
$EDITOR mailrise/mailrise.conf    # same token and chat id, same SMTP user/pass

docker compose up -d
docker compose ps                 # apprise must reach (healthy)
```

Both `.env` and `mailrise/mailrise.conf` are gitignored. The `.example` files
are tracked and must never contain a real token.

**mailrise does not expand environment variables** — it loads the file as plain
YAML, so a literal `$TELEGRAM_BOT_TOKEN` would be accepted as part of the URL
and produce an invalid Telegram target. You would discover that the first time
an alert failed to arrive, which is why the values are written in rather than
interpolated.

## Seed the apprise config key

`apprise/sentinel.cfg` is a template. The real one lives in the `apprise-config`
volume and carries the token:

```bash
docker compose exec apprise sh -c \
  'printf "tgram://%s/%s\n" "$BOT" "$CHAT" > /config/sentinel.cfg' \
  BOT=... CHAT=...
```

`sentinel notify --seed-config` writes the same content; it is an ops one-shot
the runtime never invokes.

## Verify (PLAN §3, T1)

Both must land in Telegram. Until they do, T1 is not done — the supervisor can
compute a perfect report and still never reach you.

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

## Binding

`APPRISE_BIND` and `MAILRISE_BIND` default to `127.0.0.1`. Widen to the host's
LAN address only when other machines must deliver mail here, and never to
`0.0.0.0`: anyone who can reach the apprise port can send messages as your bot.

## Operational note

Restarting this stack does not lose alerts. When apprise is unreachable the
supervisor queues to its outbox and retries; after `OUTBOX_SMTP_AFTER` failed
ticks it switches to the mailrise SMTP path for the same payload.
