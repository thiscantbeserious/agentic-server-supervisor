# Doc comments for the refactored `internal/analyze`

**Historical design record, August 2026. Not a live specification.** This was
the text applied to the package; the files themselves are now authoritative and
will drift from this document as the code changes. Kept for the rules and the
reasoning in `refactor-analyze-vocabulary.md` §3, not as a comment source of
truth, never "restore" a comment from here without checking the code first.

Only `analyze.go` carries the package doc (godoc permits one); the other seven
files get a plain comment above their `package` clause.

## analyze.go, package doc

```go
// Package analyze turns one tick's collected facts into a human-readable
// report by calling an LLM, and survives every way that call can fail.
//
// The pipeline makes at most two model calls per tick. The triage call
// receives all facts plus a window of recent reports and returns the full
// report: findings with severities, a status, a headline. If triage surfaces
// a new finding for a component that has a deep collector, the deep-dive
// call receives that one finding plus a focused deep collection and returns
// a grounded analysis and a conditional recommendation for it. If the model
// is unreachable or keeps returning garbage, a deterministic fallback report
// surfaces the raw high-priority kernel lines instead, so no hardware event
// is ever lost to an LLM outage.
//
// This package is the security boundary between attacker-controlled log
// text and an LLM prompt. Anything an attacker can write to a log on the
// monitored host ends up inside these prompts, so every payload is wrapped
// in fences marked with a fresh random nonce per run: injected text cannot
// forge a fence end it cannot predict, and the prompt instructs the model
// to treat everything inside the fences as data. The model has no tools and
// executes nothing; a successful injection is limited to wrong text in a
// report, and the recommendation field, the one field an operator might
// paste into a shell, additionally passes a deterministic deny-list guard.
//
// The binding spec is contracts/analyze.md.
package analyze
```

## File headers

```go
// agy.go: the agy subprocess. Spawning, minimal environment, output capture,
// the JSON envelope check, and the error classes that decide whether a
// failure is worth retrying. Nothing else in the package execs anything.
```

```go
// triage.go: the first model call. One attempt over all facts, one retry
// with a correction appended if the answer fails validation, and the
// classification of agy failures into fallback reasons.
```

```go
// deepdive.go: the second model call. Selecting which new finding earns the
// deep dive, the deferred-candidate queue, the focused second prompt, and
// merging the returned analysis into the triage report. Every failure on
// this path is non-fatal: the triage report is already valid and enrichment
// never becomes a gate.
```

```go
// guard.go: the deterministic deny-list over the recommendation field,
// the last check before model output reaches text an operator might paste
// into a root shell.
```

```go
// prompt.go: prompt construction. Embeds the prompt/ directory, renders the
// templates, generates the fence nonce, and keeps every prompt under the
// size at which agy silently stops answering.
```

```go
// history.go: the report window. Loads recent reports from state, projects
// them into the compact form the prompts carry, and computes which previous
// findings are resolved this tick.
```

```go
// fallback.go: the LLM-free report. Built whenever no valid model report
// could be produced, it carries the raw high-priority kernel lines so an
// analyzer outage can never hide a hardware event.
```

Each file header ends with `// The binding spec is contracts/analyze.md.`

## Exported identifiers

```go
// Run executes one analysis tick: triage call, optional deep dive, guard,
// and returns a validated report.
//
// Run returns a non-nil, valid report in every case except one: when ctx
// was cancelled it returns (nil, err) and authors nothing, because a
// shutdown is not an analyzer failure and must not fabricate an ALERT.
// Callers must nil-check before using the report. Any other non-nil error
// means the returned report is the deterministic fallback. Run never panics
// and writes only under TmpDir and the deep-dive queue in StateDir.
```

```go
// Options is Run's per-tick input: configuration, the facts collected this
// tick, and the tick sequence number stamped into the report.
```

```go
// Deps holds the two operations tests replace: running agy and collecting
// deep facts. Plain function fields, not interfaces, there is exactly one
// real implementation of each.
```

```go
// DefaultDeps wires the real agy subprocess and the in-process deep
// collector.
```

```go
// Fallback builds the report used when no valid model report exists: an
// ALERT carrying the raw high-priority kernel lines of this tick, so the
// operator still sees hardware events during an analyzer outage. code is
// the machine-readable failure reason; the report text carries only the
// mapped human phrase, because the notifier strips underscores from report
// strings and "agy_missing" would arrive as "agymissing". The result always
// passes report.Validate.
```

## agy.go

```go
// agy failure classes. The split exists because retrying is only ever
// useful when the model answered badly: a missing binary, a hard timeout,
// a non-zero exit, or an unauthenticated agy will fail identically on the
// second attempt, and retrying those only doubles the outage window.
var (
	errAgyMissing = errors.New("agy: binary not found")
	errAgyTimeout = errors.New("agy: killed by hard timeout")
	errAgyFailed  = errors.New("agy: exited non-zero or unusable")

	// errAgyUnauth: agy's stderr shows an OAuth prompt. Headless mode
	// cannot complete an OAuth flow, so this persists until a human
	// re-authenticates; the fallback names that fix instead of sending a
	// 3am reader to check a healthy binary.
	errAgyUnauth = errors.New("agy: not authenticated")

	// errAgyEmptySystemic: the envelope reports a failed call or zero
	// input tokens, the prompt never reached a model that answered, so a
	// retry would re-run the identical broken invocation. An empty response
	// from a call that did spend tokens is the one empty-output case that
	// is plausibly transient and stays retry-eligible.
	errAgyEmptySystemic = errors.New("agy: envelope reports failure or zero input tokens")
)
```

```go
// runAgy executes one agy call. The prompt travels as an argv argument, not
// stdin: agy's print mode ignores stdin entirely, and a piped-in prompt
// produces a hallucinated answer to an empty question. The prompt file is
// still written for debugging and the retry append, but it is passed by
// value. The environment is reduced to PATH, HOME, TMPDIR, TZ, LANG and
// AGY_*, never the full process environment, which carries notification
// secrets that must not leak into a subprocess.
//
// A cancelled context is reported as cancellation, checked before the
// timeout and exit-code branches, because a SIGTERM mid-call also makes
// cmd.Run return an error and must not be misread as an agy failure.
```

```go
// decodeAgyEnvelope unwraps agy's --output-format json envelope and rejects
// answers that never happened. agy has an upstream defect where print mode
// silently drops stdout in non-TTY contexts, exactly how this package
// spawns it, returning exit 0 with nothing, so a bare read cannot tell
// "no response" from "response lost". The envelope makes it distinguishable:
// a failed status or zero input tokens means the prompt never reached the
// model (not retryable); a successful, token-spending call with an empty
// response is plausibly a transient drop (retryable).
```

```go
// normalizeAgyOutput trims whitespace and strips a single leading ```json
// (or ```) fence line and trailing fence line, the two decorations models
// habitually add around JSON they were told to return bare.
```

```go
// limitedBuffer caps captured stdout, silently discarding the excess, so a
// misbehaving or malicious agy cannot grow the buffer without bound.
```

```go
// isAgyAuthFailure detects the OAuth prompt in agy's stderr. The stderr
// text itself is never logged, log lines must not carry subprocess output,
// only this in-process check reads it.
```

## triage.go

```go
// runTriage performs the first model call with one retry. The retry happens
// only when the model answered but the answer failed parsing or validation,
// or when a successful call inexplicably returned nothing; every other
// failure class would fail identically again. The retry prompt appends the
// concrete validation error from the first attempt: print mode is
// stateless, so without it "your previous answer was invalid" carries no
// information and the retry is a re-roll, not a correction.
```

```go
// agyAttempt runs one agy call and classifies the outcome. On success the
// report is non-nil; otherwise reason names the failure for the fallback,
// and for parse/validation failures err carries the raw validator message
// so the correction block can quote it verbatim.
```

```go
// buildFallback wraps Fallback with re-validation and the log line
// operators grep for during an outage.
```

`classifyAgyErr` gets no comment, the switch documents itself.

## deepdive.go

```go
// deepDiveCapable lists the components that have a deep collector.
```

```go
// isNewFinding reports whether a finding has not been seen as an active
// alert before. Any stat error counts as "new", a fresh state directory
// makes every finding new, which is the safe direction.
```

```go
// selectCandidate picks the one finding that gets this tick's deep dive:
// a previously deferred finding from the queue outranks a fresh one
// (oldest first, so deferrals cannot starve), otherwise the first new
// deep-dive-capable finding, alerts before watches, ties broken by report
// order.
```

```go
// manageDeepQueue keeps the deferred-candidate queue honest: every new
// deep-dive-capable finding that was not chosen is queued, the consumed
// candidate's entry is removed, and entries for findings no longer in the
// report are dropped as stale. Errors here never fail the tick, queue
// bookkeeping must not gate analysis, but each one leaves a log line
// rather than vanishing.
```

```go
// runDeepDive performs the second model call and merges the result into the
// triage report. Every failure path returns with the report untouched: the
// caller already holds a valid report and enrichment is never a gate.
//
// The merge trusts only our own pointer to the candidate finding, never a
// key echoed back by the model. The returned headline, when present,
// replaces the triage headline: the headline becomes the notification
// title, and triage wrote it knowing only the shallow tick facts, if the
// deep collection reveals something worse, a stale headline misleads the
// operator at exactly the wrong moment.
```

Inline, at the deep-dive log call:

```go
	// Deliberately not using "component" as the attr key: the log handler
	// diverts any attr with that exact name into the line's component slot
	// (already "analyze" for this package), which would silently replace
	// it with "zfs"/"kernel"/etc. "target" avoids the collision.
```

```go
// deepDiveResponse is the deep-dive call's answer: analysis and
// recommendation for the one candidate finding, plus an optional
// replacement headline. It is deliberately not a full report: an earlier
// version required the report shape, and the model would copy the 16-hex
// finding key with one digit wrong, silently losing the enrichment to a
// key mismatch. The response now contains nothing worth copying.
```

```go
// validateDeepDiveResponse enforces the same bounds as
// prompt/deepdive.schema.json. The schema file is what agy receives; Go
// enforces it at runtime because model output is unvalidated input either
// way. Keep the two in lockstep.
```

```go
// atomicWriteFile writes via create-temp, write, sync, close, rename, so a
// crash mid-write can never leave a torn or partial file for a later tick
// to read.
```

```go
// appendNoDeepDiveSuffix marks new findings whose component has no deep
// collector, so the operator knows the missing analysis is a capability
// gap, not an omission. The explanation is truncated first so the result
// stays within the schema's length bound. Callers skip this entirely when
// deep dives are disabled: the operator switched the feature off and does
// not need every finding annotated with that fact.
```

## guard.go

```go
// The deny-list below covers only the recommendation field, deliberately.
// Body text is narrative ("the ssh daemon logged three failed password
// attempts" is a factual report, not a proposal), and applying these
// patterns there destroyed legitimate reports on day one. A recommendation
// is different in kind: it is a command proposal a tired operator may paste
// into a root shell, so the patterns are broad and false positives are
// accepted, a suppressed suggestion costs one visible meta finding, a
// missed one can cost the host.
//
// Hard-won shape rules, each from a real false-positive class:
//   - danger tokens are word-bounded ("dd" must not match "add");
//   - a TLD-shaped suffix is only dangerous when it is not operational
//     vocabulary, on the target every systemd unit is "<name>.service",
//     and a guard that cannot say "restart smartd.service" destroys the
//     output it protects. "sh" is deliberately absent from the safe set:
//     .sh is a live TLD widely used to host payloads;
//   - interpreters (sh, bash, python, ...) are blocked alongside fetch
//     verbs, because blocking the download while allowing "sh payload.sh"
//     closes only half the path. "node" is excluded: it is ordinary
//     storage vocabulary here and absent from the target host;
//   - a redirect must have a path-shaped target: a naked ">" is the
//     comparison operator in this domain ("if cksum_errors > 1" is the
//     exact conditional shape recommendations are asked to take).
//
// This is a mitigation, not a proof: "fetch the script from evil.example
// and run it with the shell" is not catchable by substring matching. The
// residual risk is accepted because a human evaluates every recommendation
// and the supervisor itself executes nothing. Any change here must pass
// both the attack table and the operational-prose table in the tests,
// three consecutive false-positive classes came from testing against the
// attack table alone.
```

```go
// guardRecommendations blanks any recommendation matching the deny-list and
// appends one watch finding recording the withholding. The record is never
// dropped: if the report is at the findings cap, the least important
// finding is evicted to make room, losing an "all clear" line is better
// than silently losing the fact that a dangerous proposal was suppressed.
// Idempotent, and a no-op when no finding carries a recommendation.
```

```go
// recomputeStatus re-derives status from the highest finding severity.
// Appending the guard's watch finding can raise the highest severity, and
// a report whose status disagrees with its findings fails validation.
```

## prompt.go

On the embed block, this carries the `prompt/` directory charter:

```go
// The prompt/ directory holds everything this package says TO the model:
// the role document, the prompt skeleton, and the deep-dive response
// schema. No instruction, boundary paragraph or fence originates in a Go
// string literal, which is what keeps the prompt auditable as text.
//
// The payloads are a different matter and some of their bytes are
// Go-authored: the history projection's field names, the validator message
// the correction quotes, and text this package itself wrote into an
// earlier tick's report, a withheld-recommendation note, a fallback
// placeholder, which returns here as history evidence. Auditing what the
// model is told means reading this directory; auditing everything it sees
// means following the payloads too.
//
// (The one model-facing file outside this directory is report.schema.json,
// which lives in internal/report because go:embed cannot cross packages.)
//
// text/template, not html/template: HTML escaping would corrupt the
// embedded JSON payloads. Both calls share one header define so the fence
// and boundary structure exists in exactly one place and cannot silently
// diverge between the triage and deep-dive prompts.
```

```go
// roleMD is the embedded role document with its trailing newline trimmed:
// the template supplies the blank-line separator that follows it, and the
// file's own newline would double it. The prompt is compared byte-for-byte
// in tests, so this matters.
```

```go
// promptData feeds both prompt templates; fields unused by one call stay
// zero. DeepDive selects the deep-dive boundary paragraph, which names all
// three of that prompt's fenced payloads.
```

```go
// newNonce returns 16 hex chars from crypto/rand, the per-run fence
// token. The fences are only a boundary if injected log text cannot
// predict them; a fresh random nonce per run is what makes a forged
// "end of fence" line inert.
```

```go
// renderTriagePrompt renders the triage prompt: role document, security
// boundary, history window, this tick's facts, task.
```

```go
// renderDeepDivePrompt renders the deep-dive prompt: role document,
// security boundary, history window, the one candidate finding, the deep
// collection, task.
```

```go
// buildTriagePrompt renders the triage prompt and, if it exceeds the size
// budget, re-renders it from a reduced copy of the facts. agy silently
// returns an empty answer past a measured ~30 KB prompt, an order of
// magnitude below the facts size cap, so an unbudgeted prompt fails in the
// worst way: successfully, with nothing. The non-facts shell has a fixed
// size, so one render yields the exact remaining budget, no iteration.
// The reduction uses the collector's own truncation on a copy; the
// original facts are never touched, because the fallback and raw-alert
// paths read them and must see exactly what the collector emitted.
```

```go
// buildDeepDivePrompt applies the same budget technique to the deep-dive
// prompt. Not cosmetic: a deep collection can reach the full facts size
// cap, a single argv string that large fails exec outright on Linux, and
// anything past ~30 KB hits agy's silent-empty cliff, unbudgeted, the
// deep dive would fail systematically for exactly the large collections it
// exists to analyze.
```

```go
// deepCopyFacts returns a fully independent copy. The facts sections are
// pointers, so a struct copy would still alias the slices the budget
// reduction mutates.
```

```go
// buildCorrection renders the retry suffix with the concrete validation
// error from the failed attempt (truncated; the text comes from our own
// validator and contains no log content, so it is safe to embed).
```

## history.go

```go
// historyFinding is the compact projection of a past finding carried in the
// prompt. Evidence, occurrences and first_seen are load-bearing: the dedup
// key deliberately masks digits, so "cksum_errors=1" and "cksum_errors=7"
// share a key, the key alone proves recurrence but can never prove
// growth. The model is asked to compare counters across ticks; without the
// evidence text that comparison is impossible and it answers from
// imagination.
```

```go
// loadHistoryReports returns the newest n reports from the state history,
// oldest first. Filenames sort chronologically as strings by construction.
// Only *.json is read: atomic writes leave .tmp-* files in the same
// directory, and letting one into the window would evict a real report.
// Unreadable or unparseable files are skipped; a missing directory yields
// nil.
```

```go
// computeResolved returns which of the previous report's findings are gone
// this tick, as evidence snippets. Computed in Go, overwriting whatever the
// model emitted: set arithmetic over data we already hold does not belong
// in a probabilistic component. Only the newest report is compared,
// anything older was already announced resolved. Entries are truncated to
// the schema's length bound and empty results skipped, since one overlong
// or empty entry would invalidate the whole report.
```

```go
// historyProjectionLines renders each history report as one compact JSON
// line for the prompt.
```

```go
// truncRunes keeps at most n leading runes.
```

`newestHistory` gets no comment, three obvious lines.

## fallback.go

```go
// reasonPhrase maps machine failure codes to the human phrases the report
// carries. The code goes to the log; only the phrase enters report text,
// because the notifier strips underscores from report strings and
// "agy_missing" would reach the operator as "agymissing".
```

```go
// protectedKernelLines returns this tick's high-priority kernel lines,
// oldest first, capped, keeping the newest when the cap binds. A forward
// walk that stops at the limit would keep the oldest lines and drop the
// incident happening right now.
```

```go
// truncLinesKeepNewest fits lines into a rune budget by dropping whole
// lines from the oldest end, never splitting a line, so the newest line
// survives whenever the budget binds. If even the single newest line does
// not fit, its trailing runes are kept.
```

```go
// truncRunesSuffix keeps at most n trailing runes.
```

`reverseStrings` gets no comment.

## analyze.go, remaining

```go
// logWriter is where this package's log output goes. Tests point it at a
// buffer to assert on the exact lines the spec fixes; the zero value means
// stderr.
```

```go
	// Seq is deliberately not threaded through: deep facts' tick number is
	// informational and nothing keys on it.
```

`newLogger` needs no comment beyond the `logWriter` doc.

## prompt/prompt.tmpl, leading comment, renders nothing

```
{{/*
The complete prompt skeleton for both model calls, in render order:
security-boundary paragraphs, shared header, triage task, deep-dive task,
retry correction. Every substitution point in every prompt is in this file;
role.md and deepdive.schema.json beside it hold the rest of the fixed text.
Rendered output is compared byte-for-byte against testdata goldens, and
whitespace inside a define is load-bearing, text between defines is not,
since it belongs to the file template and is never rendered.
*/}}
```

`role.md` gets no charter comment: its bytes are model-visible and normative,
so it must stay exactly the role text.

## Delete outright

- `analyze.go:37`, the comment whose entire content was a citation.
- `analyze.go:94–103`, the `errAgyEmptySystemic` review-round narrative. The
  reasoning survives above; the process history belongs to git.
- `analyze.go:106–114 / 127–133 / 169–177 / 205–213`, duplicated cancellation
  and retry rationales, now stated once each at `Run` and `runTriage`.
- `analyze.go:516–517`, a doubled `runAgy` doc comment (merge artifact).
- `analyze.go:556–560, 570–572`, paragraphs restating the error-class block.
  Keep one line at the branch: `// SIGTERM also surfaces as a non-nil cmd.Run
  error; check cancellation first.`
- `prompt.go:26–33, 87–97, 193–199, 219–226`, byte-diff archaeology, commit
  references, citation framing. Reasons kept above.
- `stage2.go:74–105`, the round-by-round changelog, compressed into the guard
  block above.
- `deep.go:12, 17, 23, 101–107, 158–163` and `fallback.go:12–13, 20–21, 80–82`
 , citations inside otherwise good comments; prose keeps the content.
