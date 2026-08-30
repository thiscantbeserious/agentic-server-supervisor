# Writing Rules

How everything in this repository is written: code comments, documentation,
commit messages, contract text. Applies to every agent and to every human.

## The rule

**Write for someone reading this code a year from now who was not here.**
They do not know which milestone produced a line, which review round found a
defect, or who asked for a change. None of that helps them; all of it costs
them attention on the way to what they actually need.

A comment earns its place by explaining **why the code is the way it is**,
the constraint, the measurement, the failure it prevents. Not by recording
how it came to be written.

## Never reference

- **Milestones**: `T1`, `T7`, "in the next TODO", "deferred to T8".
- **Process**: "round 3 review", `MUST-FIX 2`, "the reviewer's finding",
  "main's explicit ask", "their own catch", "found by the agy gate".
- **People and roles**: who proposed, who implemented, who approved.
- **Chronology**: "the previous version", "an earlier draft said", "used to
  be", "now changed to", unless the old behaviour is still reachable in
  the wild and the reader must recognise it.

Git history holds all of this, keyed to the exact lines, and holds it better
than prose can. A comment repeating it is a worse copy that goes stale.

## Pull requests

PR descriptions use `.github/pull_request_template.md`. Fill its sections,
delete the ones that do not apply, put verbatim evidence in fenced blocks.
Do not invent a per-PR structure.

## Instead

Name the constraint and its consequence:

```go
// Bad , process archaeology
// Round-4 review DEFECT (their own proposal, their own catch): the previous
// count guard was `grep -c ""$1""`, which main asked us to fix in item 3.

// Good, the reason, and how to recognise the trap
// Quote the key explicitly rather than by adjacent string concatenation:
// `""$1""` collapses to an unquoted `$1`, and `grep -c` counts matching
// LINES, so on a minified document it can only ever return 0 or 1.
```

```markdown
Bad:   The notification stack (T1) and, from T7, the supervisor service.
Good:  The notification stack. The supervisor service is added alongside it
       once the image exists.
```

**The measurement is worth keeping; the round that produced it is not.**
"Verified against real msmtp on Debian 13" belongs in the comment. "Round-2
blocker 1" does not.

## Naming carries the weight

Anything a comment has to explain about *what* something is should be in the
name instead. A comment explaining that `stage2` means the deep-dive call is
a name that failed. Rename it, delete the comment.

The corollary: when a name is right, most comments become unnecessary, and
the ones that remain are all about *why*, which is the only thing a reader
cannot recover from the code itself.

## Where milestone references legitimately live

`PLAN.md`, it *is* the milestone plan. Nothing else.

Contracts describe how the system behaves, not when it was built. A contract
sentence that only makes sense if you know what T5 was is a sentence that
needs rewriting.

## Diagrams

Mermaid, never ASCII art.

## Language

English, everywhere, regardless of the language a conversation happens in.
