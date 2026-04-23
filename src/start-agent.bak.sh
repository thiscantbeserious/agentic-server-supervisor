#!/bin/bash
# /etc/gemini-watcher/start-agent.sh

SESSION_FILE="/etc/gemini-watcher/session_id"
LOG="/etc/gemini-watcher/acp.log"

source /etc/gemini-watcher/gemini-sentinel.env
export GEMINI_API_KEY

PIPE_IN=$(mktemp -u)
PIPE_OUT=$(mktemp -u)
mkfifo "$PIPE_IN" "$PIPE_OUT"

stdbuf -oL /usr/bin/gemini --experimental-acp --approval-mode yolo \
  --model gemini-2.5-flash \
  --allowed-tools="df,free,uptime,cat,head,tail,grep,wc,ls,find,journalctl,ss,ip,ping,dig" \
  < "$PIPE_IN" > "$PIPE_OUT" 2>>"$LOG" &
GEMINI_PID=$!

exec 3>"$PIPE_IN"
exec 4<"$PIPE_OUT"

read_jsonrpc() {
  local timeout="${1:-120}"
  while read -t "$timeout" line <&4; do
    if [[ "$line" == '{"jsonrpc"'* ]]; then
      echo "$line"
      return 0
    fi
  done
  return 1
}

# 1. Initialize
echo "[$(date)] Sending initialize..." >> "$LOG"
echo '{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":1}}' >&3
INIT=$(read_jsonrpc 120)
echo "[$(date)] INIT: $INIT" >> "$LOG"

# 2. Authenticate
echo "[$(date)] Sending authenticate..." >> "$LOG"
echo '{"jsonrpc":"2.0","method":"authenticate","id":2,"params":{"methodId":"gemini-api-key"}}' >&3
AUTH=$(read_jsonrpc 120)
echo "[$(date)] AUTH: $AUTH" >> "$LOG"

# Check auth success
AUTH_ERROR=$(echo "$AUTH" | jq -r '.error.message // empty')
if [[ -n "$AUTH_ERROR" ]]; then
  echo "[$(date)] FATAL: Auth failed: $AUTH_ERROR" >> "$LOG"
  exit 1
fi

# 3. Create session
echo "[$(date)] Sending session/new..." >> "$LOG"
echo '{"jsonrpc":"2.0","method":"session/new","id":3,"params":{"cwd":"/etc/gemini-watcher","mcpServers":[]}}' >&3
SESSION=$(read_jsonrpc 120)
echo "[$(date)] SESSION: $SESSION" >> "$LOG"

SESSION_ID=$(echo "$SESSION" | jq -r '.result.sessionId // empty')
if [[ -z "$SESSION_ID" ]]; then
  echo "[$(date)] FATAL: No sessionId" >> "$LOG"
  exit 1
fi

echo "$SESSION_ID" > "$SESSION_FILE"
echo "[$(date)] Session created: $SESSION_ID" >> "$LOG"

# 4. Send initial prompt - note: prompt is an ARRAY of parts
echo "[$(date)] Sending initial prompt..." >> "$LOG"
jq -nc \
  --arg sid "$SESSION_ID" \
  '{"jsonrpc":"2.0","method":"session/prompt","id":4,"params":{"sessionId":$sid,"prompt":[{"type":"text","text":"Begin monitoring loop as per GEMINI.md"}]}}' >&3

# 5. Read all responses (streaming) in background
while read -t 300 line <&4; do
  echo "[$(date)] [AGENT] $line" >> "$LOG"
done &
READER_PID=$!

# 6. Listen on control pipe for commands
while IFS= read -r cmd < /etc/gemini-watcher/control_pipe; do
  echo "[$(date)] [CTRL] $cmd" >> "$LOG"
  echo "$cmd" >&3
done

wait "$READER_PID"
exec 3>&-
exec 4<&-
kill "$GEMINI_PID" 2>/dev/null
rm -f "$PIPE_IN" "$PIPE_OUT"
