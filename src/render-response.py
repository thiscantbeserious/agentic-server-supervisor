#!/usr/bin/env python3
# /etc/gemini-watcher/render-response.py
import sys
import json
from rich.console import Console
from rich.markdown import Markdown

console = Console()
verbose = "--verbose" in sys.argv or "-v" in sys.argv

thoughts = []
messages = []

for line in sys.stdin:
    line = line.strip()
    if line == "DONE":
        break
    if not line.startswith('{"jsonrpc"'):
        continue
    try:
        data = json.loads(line)
    except json.JSONDecodeError:
        continue

    error = data.get("error", {}).get("message")
    if error:
        console.print(f"[red]❌ {error}[/red]")
        break

    update = data.get("params", {}).get("update", {})
    update_type = update.get("sessionUpdate", "")

    content = update.get("content", {})
    if isinstance(content, list):
        text = "".join(c.get("text", "") for c in content if isinstance(c, dict))
    elif isinstance(content, dict):
        text = content.get("text", "")
    else:
        text = ""

    if update_type == "agent_thought_chunk":
        thoughts.append(text)
    elif update_type == "agent_message_chunk":
        messages.append(text)
    elif update_type == "tool_call" and verbose:
        title = update.get("title", "")
        status = update.get("status", "")
        if title and status == "in_progress":
            console.print(f"[yellow]🔧 {title}[/yellow]")
    elif update_type == "tool_call_update" and verbose:
        status = update.get("status", "")
        content = update.get("content", [])
        if status == "completed" and content:
            tool_text = content[0].get("content", {}).get("text", "")
            if tool_text:
                console.print(f"[dim]   → {tool_text[:200]}[/dim]")

if verbose and thoughts:
    thought_text = "".join(thoughts)
    console.print()
    console.print("💭 [dim]Thoughts:[/dim]")
    console.print("[dim]───[/dim]")
    console.print(Markdown(thought_text), style="dim")
    console.print("[dim]───[/dim]")
    console.print()

if messages:
    message_text = "".join(messages)
    console.print(Markdown(message_text))
