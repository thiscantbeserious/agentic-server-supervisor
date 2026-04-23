#!/bin/bash
# /etc/gemini-watcher/probe-raw.sh

source /etc/gemini-watcher/gemini-sentinel.env
export GEMINI_API_KEY

{
  sleep 20
  echo '{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":1}}'
  sleep 5
  echo '{"jsonrpc":"2.0","method":"session/new","id":2,"params":{"cwd":"/tmp","mcpServers":[]}}'
  sleep 5
} | stdbuf -oL /usr/bin/gemini --experimental-acp --approval-mode yolo > /tmp/gemini-raw-out.txt 2>/tmp/gemini-raw-err.txt

echo "=== STDOUT ==="
cat /tmp/gemini-raw-out.txt
echo ""
echo "=== STDERR ==="
cat /tmp/gemini-raw-err.txt
