#!/bin/bash
# /etc/gemini-watcher/send-prompt.sh

VERBOSE=""
if [[ "$1" == "--verbose" || "$1" == "-v" ]]; then
  VERBOSE="-v"
  shift
fi

SESSION_ID=$(cat /etc/gemini-watcher/session_id)
PROMPT="$*"
REQ_ID=$(date +%s%N | cut -c1-13)
RESP_DIR="/etc/gemini-watcher/responses"
RESP_FILE="$RESP_DIR/$REQ_ID"
TIMEOUT="${GEMINI_TIMEOUT:-120}"

cleanup() {
  pkill -f "tail -f $RESP_FILE" 2>/dev/null
  rm -f "$RESP_FILE"
}
trap cleanup EXIT INT TERM

touch "$RESP_FILE"

jq -nc \
  --arg sid "$SESSION_ID" \
  --arg prompt "$PROMPT" \
  --argjson id "$REQ_ID" \
  '{"jsonrpc":"2.0","method":"session/prompt","id":$id,"params":{"sessionId":$sid,"prompt":[{"type":"text","text":$prompt}]}}' \
  > /etc/gemini-watcher/control_pipe

timeout "$TIMEOUT" tail -f "$RESP_FILE" | python3 /etc/gemini-watcher/render-response.py $VERBOSE
