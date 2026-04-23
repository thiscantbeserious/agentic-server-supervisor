#!/bin/bash
# /etc/gemini-watcher/start-agent.sh

SESSION_FILE="/etc/gemini-watcher/session_id"
LOG="/etc/gemini-watcher/acp.log"
RESP_DIR="/etc/gemini-watcher/responses"
REQ_ID_FILE="/etc/gemini-watcher/current_req_id"

mkdir -p "$RESP_DIR"

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
  while read -r -t "$timeout" line <&4; do
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
echo '{"jsonrpc":"2.0","method":"authenticate","id":2,"params":{"methodId":"oauth-personal"}}' >&3
AUTH=$(read_jsonrpc 120)
echo "[$(date)] AUTH: $AUTH" >> "$LOG"

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

# 4. Send initial prompt
echo "[$(date)] Sending initial prompt..." >> "$LOG"
echo "" > "$REQ_ID_FILE"
jq -nc \
  --arg sid "$SESSION_ID" \
  '{"jsonrpc":"2.0","method":"session/prompt","id":4,"params":{"sessionId":$sid,"prompt":[{"type":"text","text":"You are now online. Await ticks and commands."}]}}' >&3

# 5. Background reader — writes to regular files
while IFS= read -r -t 600 line <&4; do
  [[ "$line" != '{"jsonrpc"'* ]] && continue
  echo "[$(date)] [AGENT] $line" >> "$LOG"

  CUR_ID=$(cat "$REQ_ID_FILE" 2>/dev/null)

  # Direct response (has id field)
  RESP_ID=$(echo "$line" | jq -r '.id // empty' 2>/dev/null)
  if [[ -n "$RESP_ID" && -f "$RESP_DIR/$RESP_ID" ]]; then
    echo "$line" >> "$RESP_DIR/$RESP_ID"
    STOP=$(echo "$line" | jq -r '.result.stopReason // empty' 2>/dev/null)
    if [[ -n "$STOP" ]]; then
      echo "DONE" >> "$RESP_DIR/$RESP_ID"
      echo "" > "$REQ_ID_FILE"
    fi
    continue
  fi

  # Streaming update — route to current request file
  if [[ -n "$CUR_ID" && -f "$RESP_DIR/$CUR_ID" ]]; then
    echo "$line" >> "$RESP_DIR/$CUR_ID"
  fi

done &
READER_PID=$!

# 6. Listen on control pipe
while IFS= read -r cmd < /etc/gemini-watcher/control_pipe; do
  echo "[$(date)] [CTRL] $cmd" >> "$LOG"

  REQ_ID=$(echo "$cmd" | jq -r '.id // empty' 2>/dev/null)
  if [[ -n "$REQ_ID" ]]; then
    echo "$REQ_ID" > "$REQ_ID_FILE"
  fi

  echo "$cmd" >&3
done

wait "$READER_PID"
exec 3>&-
exec 4<&-
kill "$GEMINI_PID" 2>/dev/null
rm -f "$PIPE_IN" "$PIPE_OUT"
