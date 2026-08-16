# Refactor: analyze vocabulary, layout and documentation

Working spec for the `refactor-analyze-vocabulary` branch. It records four
passes of design decisions so the reasoning survives the merge. Delete this
file once the contract amendments have landed — `contracts/analyze.md` is the
durable record; this is scaffolding.

**Nothing here changes what the program computes.** Rendered prompt bytes,
report contents, state layout and dedup keys are all unchanged. The only
observable changes are log/error strings and two schema descriptions renamed
to the new vocabulary, listed in §5.

## 1. Vocabulary

The package described its two LLM calls as "stage 1" and "stage 2" — ordinal
names a reader cannot decode. Replaced by the domain words:

| Old | New | Why |
|---|---|---|
| stage 1 | **triage** | It sweeps every fact once, assigns each anomaly a severity, rolls up a status, and thereby decides what earns deeper attention. That is triage in both the medical and incident-response senses. |
| stage 2 | **deep dive** | Not invented: the contract already said "deep-dive selection", `deep-queue/`, `DEEP_ENABLED`, `DEEP_TIMEOUT`, the `DEEP` fence; the code already had `runDeepDive`, `deepDiveCapable`. Only the artifacts were named "stage2". |

`deep dive` was already contract vocabulary; `stage 1`/`stage 2` was jargon
layered on top of it. The contract is therefore amended too — it was the source
of the bad naming, not a victim of it.

**Naming principle**, applicable beyond this package: *name every unit by its
domain role in the vocabulary the binding contract already uses — never by
ordinal position, layer cliché, or build order.* `collect`, `state` and
`notify` already obeyed this; `analyze` was the only offender.

## 2. Layout

```
internal/analyze/
├── analyze.go        Run, Options, Deps, DefaultDeps — tick orchestration only
├── agy.go            the agy subprocess: exec, env, envelope, error classes
├── triage.go         call 1: runTriage, agyAttempt, classifyAgyErr
├── deepdive.go       call 2 whole: execution, selection, queue, validation
├── guard.go          recommendation deny-list + recomputeStatus
├── prompt.go         embeds, nonce, renderers, budget builders
├── history.go        report window, projection, computeResolved
├── fallback.go       unchanged
├── prompt/
│   ├── role.md                   was sentinel.md, bytes untouched
│   ├── prompt.tmpl               was 5 × templates/*.tmpl + the correction literal
│   └── deepdive.schema.json      was stage2.schema.json
└── testdata/         three goldens renamed, contents byte-identical
```

Deleted: `templates/` (5 files → 1), `stage2.go`, `deep.go`, `sentinel.md`,
`stage2.schema.json`.

Three things this asserts that the old tree did not:

- **The deep dive has one location.** It was smeared across three files —
  selection in `deep.go`, execution buried mid-`analyze.go`, validation in
  `stage2.go`. `stage2.go` additionally hid the recommendation guard, which
  runs on the triage-only path too and has nothing to do with call 2.
- **`prompt/` is a security charter, not filing.** Its contents are exactly the
  model-facing bytes. The single exception is `report.schema.json` in
  `internal/report`, because `go:embed` cannot cross packages; that is stated
  in the charter comment rather than left as a silent gap.
- **`analyze.go` shrinks to what its name claims.** It was 607 lines because it
  also carried subprocess plumbing and the whole deep-dive execution.

### Decisions taken and rejected

- **`prompt.go` keeps its name.** An earlier proposal renamed it `assemble.go`;
  `prompt.go` names the artifact, `assemble` names a gerund.
- **No package split.** Nothing outside the package consumes anything but
  `Run`, `Options`, `Deps`, `DefaultDeps`, `Fallback` (verified: only
  `cmd/sentinel/analyze.go`). A split would export `promptData`, the validators
  and the guard purely to serve a sibling package, and the same-package test
  file exercises those internals against one shared fixture set.
- **Assets consolidate by content kind, not by extension.** Making everything
  `.tmpl` (role as a `{{define}}`) would break byte-comparability with the
  contract clause that reproduces the role document verbatim, and stop markdown
  tooling rendering the one security-audited prose file. Making everything `.md`
  would misname Go template source. So: prose stays `.md`, the five template
  fragments become one `prompt.tmpl` with defines in render order, and the
  schema joins them because it is model-facing bytes.
- **`buildCorrectionBlock`'s Go string literal moves into `prompt.tmpl`.** After
  this, no *fixed* prompt text — no instruction, boundary paragraph or fence —
  lives in a Go string literal. Variable payloads are still marshalled from Go
  (the facts and finding JSON, the history projection's field names, the
  validator message the correction quotes); the claim is about the skeleton, not
  about every byte. The truncation of the validation error also stays in Go —
  that is logic, not text.

## 3. Documentation rules

1. A comment says what the thing does and why it exists — never which clause
   mandated it. No `§`, `C<n>`, `A<n>`, `D<n>`, `step N`, test-case numbers, or
   review-round archaeology anywhere in code comments; git history owns that.
2. Where a comment was only a citation, the rewrite states the actual reason
   that clause contains.
3. Never restate the signature, and no filler.
4. Plain technical English. No marketing adjectives, no hedging, no slang.
5. Go convention: the comment begins with the identifier's name, except file
   headers, which are plain prose.
6. Shorter is better, but a hard-won reason is never deleted to get there.

**Traceability:** one trailing `// The binding spec is contracts/analyze.md.`
per file header, never inline. The spec→code direction is already carried by
the contract's file-layout section; the reverse costs one line a reviewer needs
and a reader can ignore.

**Knowledge that must survive the rewrite**, restated as reasons rather than
citations: why a dead binary is never retried; why `component` is deliberately
not a slog attr key; why the deep dive has its own schema rather than the report
schema; why the nonce fences exist; and every false-positive class encoded in
the recommendation guard.

## 4. Safety

Byte-neutrality of rendered prompts is proven by `TestPromptGoldenFiles` and
`TestPromptGoldenFiles_EmptyHistory`. **Golden file contents do not change and
must not be regenerated** — their only occurrences of "stage" are ordinary
English from the role prose ("the analysis stage of a supervisor"), not the
jargon. Only the three golden filenames change.

Template names come from `{{define}}`, not filenames, so consolidating five
files into one does not touch any `ExecuteTemplate` call. Whitespace inside the
defines is load-bearing; the goldens fail on the first stray byte.

`template.ParseFS(templatesFS, "templates/*.tmpl")` becomes
`"prompt/prompt.tmpl"`. Getting this wrong panics at init on the first test, so
it cannot ship silently.

## 5. Observable string renames (approved)

Nothing in the repo parses any of these — verified by grep. No on-disk state, no
dedup key, no metric derives from them. No golden regeneration.

- Three stderr line shapes: `stage1 attempt=… rc=…` → `triage attempt=… rc=…`,
  `stage1 invalid, retrying` → `triage invalid, retrying`, and 12 occurrences of
  `deep-dive failed, keeping stage1` → `deep-dive failed, keeping triage report`.
- slog messages `"stage1"` → `"triage"`.
- Validator error prefixes `stage2: …` → `deepdive: …`.
- Temp filename `stage2.schema-%d.json` → `deepdive.schema-%d.json`.
- `internal/report/report.schema.json` descriptions `"Stage 2 only: …"` →
  `"Deep dive only: …"` (model-visible bytes, semantically inert).

## 6. Contract amendments

`contracts/analyze.md` — the `stage 1`/`stage 2` vocabulary throughout, the
stderr example lines, the §7 template headings, the role document path
(including the §7.3 heading at :466 and the test-table row at :701), and the
file-layout tree. `CONTRACTS.md` — the embedded-assets path in C1, the package
layout line, **and C8 at :228** ("Deep context for stage 2 is `collect.Run`
with `DeepComponent` set"), which is live jargon in a binding convention.
`contracts/runtime.md` — the role document filename. `PLAN.md` — "two-stage
analysis" wording. `internal/report/report_test.go:37` — the `"stage-2 fields"`
case name. `ARCHITECTURE.md` needs **nothing**; it already used the vocabulary
being adopted.

**`stage` is overloaded in this repo — three uses must NOT be renamed:**
`contracts/runtime.md:33,40` (`Stage 1 — builder` / `Stage 2 — runtime`) are
Docker build stages. `PLAN.md:22,35,47` and `CONTRACTS.md:236,239` say
"implementation stage" in the TODO sense. And `role.md` contains "the analysis
stage of a read-only server supervisor" — ordinary English, and its bytes are
inside all three goldens. A blanket `sed` fails the golden tests loudly, but
would corrupt the documents silently.

Spotted while reading, unrelated to the rename but worth fixing in the same
pass: the contract's temp-file cleanup sentence describes a glob that does not
match the files actually created. Cleanup is list-based, so the code is correct
and the sentence is wrong.
