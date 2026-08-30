#!/usr/bin/env python3
"""Convert ```mermaid fences in markdown to image-plus-collapsed-source.

Reads markdown on stdin, writes it on stdout with every mermaid fence
replaced by a mermaid.ink-rendered image followed by the original fence
inside <details>. The GitHub mobile app does not render mermaid fences;
the image renders everywhere, the collapsed source stays editable.

Usage: gh pr view N --json body -q .body | .github/render-mermaid.py | gh pr edit N --body-file -

Idempotent: fences already inside a <details> block emitted by this
script are left alone.
"""
import base64
import json
import re
import sys
import zlib

MARKER = "<!-- rendered-mermaid -->"


def ink_url(code: str) -> str:
    state = json.dumps({"code": code, "mermaid": {"theme": "default"}})
    comp = zlib.compressobj(9, zlib.DEFLATED, 15)
    data = comp.compress(state.encode()) + comp.flush()
    pako = base64.urlsafe_b64encode(data).decode().rstrip("=")
    # /img (server-side browser screenshot), not /svg: the SVG endpoint
    # embeds text metrics for a font the viewer may not have, so labels
    # overflow and clip at node edges.
    return f"https://mermaid.ink/img/pako:{pako}?type=png"


def convert(md: str) -> str:
    def repl(m: re.Match) -> str:
        code = m.group(1)
        return (
            f"{MARKER}\n![diagram]({ink_url(code)})\n"
            f"<details><summary>Mermaid source</summary>\n\n"
            f"```mermaid\n{code}\n```\n\n</details>"
        )

    out = []
    # split on already-converted blocks so they are never re-wrapped
    parts = re.split(rf"({re.escape(MARKER)}\n.*?</details>)", md, flags=re.S)
    for part in parts:
        if part.startswith(MARKER):
            out.append(part)
        else:
            out.append(re.sub(r"```mermaid\n(.*?)\n```", repl, part, flags=re.S))
    return "".join(out)


if __name__ == "__main__":
    sys.stdout.write(convert(sys.stdin.read()))
