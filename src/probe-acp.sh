#!/bin/bash
# /etc/gemini-watcher/probe-auth.sh

source /etc/gemini-watcher/gemini-sentinel.env
export GEMINI_API_KEY

PIPE_IN=$(mktemp -u)
PIPE_OUT=$(mktemp -u)
mkfifo "$PIPE_IN" "$PIPE_OUT"

stdbuf -oL /usr/bin/gemini --experimental-acp --approval-mode yolo < "$PIPE_IN" > "$PIPE_OUT" 2>/tmp/gemini-stderr.log &
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
echo '{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":1}}' >&3
INIT=$(read_jsonrpc 120)
echo "INIT: $INIT"

# 2. Authenticate with API key
# Try different auth method calls
ID=2
for method in "auth/select" "auth/authenticate" "auth/login" "authenticate" "session/authenticate"; do
  for params in \
    "{\"authMethodId\":\"gemini-api-key\"}" \
    "{\"id\":\"gemini-api-key\"}" \
    "{\"method\":\"gemini-api-key\"}" \
    "{\"authMethod\":\"gemini-api-key\",\"apiKey\":\"$GEMINI_API_KEY\"}"; do

    ID=$((ID + 1))
    MSG="{\"jsonrpc\":\"2.0\",\"method\":\"$method\",\"id\":$ID,\"params\":$params}"
    echo "$MSG" >&3

    RESP=$(read_jsonrpc 5)
    if [[ -z "$RESP" ]]; then
      echo "⏳ $method — no response"
    else
      ERROR=$(echo "$RESP" | jq -r '.error.message // empty')
      if [[ "$ERROR" == *"Method not found"* ]]; then
        echo "❌ $method"
        break  # skip other param shapes for this method
      elif [[ -n "$ERROR" ]]; then
        echo "⚠️  $method EXISTS — error: $ERROR"
        echo "   params: $params"
      else
        echo "✅ $method — SUCCESS: $RESP"
        echo "   params: $params"
      fi
    fi
  done
done

echo ""
echo "=== STDERR ==="
cat /tmp/gemini-stderr.log

exec 3>&-
exec 4<&-
kill "$GEMINI_PID" 2>/dev/null
rm -f "$PIPE_IN" "$PIPE_OUT"
