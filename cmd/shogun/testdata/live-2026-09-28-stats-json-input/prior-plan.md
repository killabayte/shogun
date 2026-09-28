---
title: Add a `--json` flag to `shogun stats`.
plan_id: 20260928-094913-add-a-json-flag-to-shogun-stats-6be3
revision: 1
created: "2026-09-28"
updated: "2026-09-28"
status: planned
run_id: 20260928-094913-add-a-json-flag-to-shogun-stats-6be3
project: shogun
repos:
    - shogun
tags:
    - shogun
    - plan
planner: claude/opus:high
reviewer: codex/gpt-6-astra:high
---

<!-- shogun:plan:begin -->

# Add a `--json` flag to `shogun stats`.

## Goal and success criteria

Task, verbatim:

> Add a `--json` flag to `shogun stats`.
> 
> With `--json`, `shogun stats` prints the same data as its table and totals as one JSON document on stdout instead of the table: one object per run (id, status, mode, planner, reviewer, attempts, active seconds, input / cache read / cache write / output / reasoning tokens, Claude list-price equivalent, usage state, and the damage reason for a damaged run) plus the totals (runs by status, damaged count, attempts, active seconds, token sums, cost, and whether the totals are lower bounds). Without the flag the output stays exactly as it is now. The command stays read-only.

- **R-001** `shogun stats --json` prints exactly one JSON document on stdout instead of the table and totals lines, and exits 0.
  - R-001.C1: On the five-run test fixture, stdout of `stats --json` decodes as one JSON object with keys `runs` and `totals` and nothing but whitespace after it; it contains no `RUN` header or `total:` line; exit code is 0 and stderr is empty.
- **R-002** Each run is one object in `runs` (sorted by id as in the table) with id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage; a damaged run's object carries id, status "damaged" and the damage reason in `damaged`.
  - R-002.C1: For the fixture's approved run the object has status "approved", mode "fast", planner "claude/opus:high", reviewer "codex/gpt-6-astra:high", attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5, usage "ok"; the paused run has usage "unknown".
  - R-002.C2: The fixture's damaged run (20260927-000003-c) has status "damaged" and a non-empty `damaged` string, and no counter fields; non-damaged runs have no `damaged` key.
- **R-003** `totals` carries runs by status, damaged count, attempts, active seconds, the five token sums, cost, and whether the totals are lower bounds, computed by the same rules as the text totals.
  - R-003.C1: For the fixture, totals has by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5.
  - R-003.C2: For the fixture, spend_lower_bound (attempts and active time, true when damaged > 0) and usage_lower_bound (tokens and cost, true when any run's usage is not ok or damaged > 0) are both true.

## Scope and non-goals

- **R-004** (constraint) Without `--json` the output of `shogun stats` stays exactly as it is now.
  - R-004.C1: The existing TestStatsCountsEveryRunAndFlagsMissingUsage text assertions pass without modification.
  - R-004.C2: `shogun stats --dir <same runs dir>` output captured before and after the change is byte-identical (diff is empty).
- **R-005** (constraint) `shogun stats` (with or without `--json`) stays read-only.
  - R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before.

## Inputs and versions

- repo-1: `shogun` at `d85b371d6d65`, clean

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | `shogun stats --json` prints exactly one JSON document on stdout instead of the table and totals lines, and exits 0. | R-001.C1: On the five-run test fixture, stdout of `stats --json` decodes as one JSON object with keys `runs` and `totals` and nothing but whitespace after it; it contains no `RUN` header or `total:` line; exit code is 0 and stderr is empty. | task |
| R-002 | functional | true | Each run is one object in `runs` (sorted by id as in the table) with id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage; a damaged run's object carries id, status "damaged" and the damage reason in `damaged`. | R-002.C1: For the fixture's approved run the object has status "approved", mode "fast", planner "claude/opus:high", reviewer "codex/gpt-6-astra:high", attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5, usage "ok"; the paused run has usage "unknown".; R-002.C2: The fixture's damaged run (20260927-000003-c) has status "damaged" and a non-empty `damaged` string, and no counter fields; non-damaged runs have no `damaged` key. | task, repo-1 |
| R-003 | functional | true | `totals` carries runs by status, damaged count, attempts, active seconds, the five token sums, cost, and whether the totals are lower bounds, computed by the same rules as the text totals. | R-003.C1: For the fixture, totals has by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5.; R-003.C2: For the fixture, spend_lower_bound (attempts and active time, true when damaged > 0) and usage_lower_bound (tokens and cost, true when any run's usage is not ok or damaged > 0) are both true. | task, repo-1 |
| R-004 | constraint | true | Without `--json` the output of `shogun stats` stays exactly as it is now. | R-004.C1: The existing TestStatsCountsEveryRunAndFlagsMissingUsage text assertions pass without modification.; R-004.C2: `shogun stats --dir <same runs dir>` output captured before and after the change is byte-identical (diff is empty). | task |
| R-005 | constraint | true | `shogun stats` (with or without `--json`) stays read-only. | R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before. | task |

## Decisions and assumptions

- Q-001 (assumption): With --json and an empty runs directory, should stats print a JSON document with an empty runs list and zero totals, or keep today's behaviour (a 'no runs in …' note on stderr and nothing on stdout)? → Print the JSON document with an empty runs array and zero totals (no stderr note), so stdout always holds one JSON document; easy to switch.

## Context

- FACT-001 `repo-1:cmd/shogun/stats.go:39-44` (observation): cmdStats defines its flags on a FlagSet (only --dir today) and parses them with fs.Parse; there are no positional arguments. — “fs := a.newFlagSet("stats")
	dir := fs.String("dir", "", ...)
	if err := fs.Parse(args); err != nil {”
- FACT-002 `repo-1:cmd/shogun/stats.go:53-63` (observation): Runs are collected via readRunStats, sorted by id; an empty runs directory prints a note to stderr and exits 0; an unreadable runs directory is an error (exit 2). — “if len(runs) == 0 {
		fmt.Fprintf(a.stderr, "no runs in %s\n", root)
		return ExitOK”
- FACT-003 `repo-1:cmd/shogun/stats.go:65-92` (observation): The table prints per run: id, status, mode, planner, reviewer, attempts, active minutes, input, cache read, cache write, output, reasoning tokens, cost and usage state; damaged runs show status 'damaged', dashes and the damage reason, and are excluded from the sums and from byStatus. — “fmt.Fprintf(tw, "%s\tdamaged\t-\t-...\t%s\n", r.id, r.damaged)”
- FACT-004 `repo-1:cmd/shogun/stats.go:103-116` (observation): Totals have two lower-bound conditions: attempts/active time are 'at least' when damaged > 0; tokens and cost are 'at least' when any run has usage incomplete/unknown or damaged > 0. — “spendAtLeast := "" // a damaged run's attempts and time are missing from the totals too
	if damaged > 0 {”
- FACT-005 `repo-1:cmd/shogun/stats.go:26-35` (observation): Usage state per run is computed by runStats.usage(): ok, incomplete or unknown; it dereferences r.st and so must only be called for non-damaged runs. — “func (r runStats) usage() string {”
- FACT-006 `repo-1:cmd/shogun/other.go:134,154-157` (observation): The existing `status --json` precedent: a bool flag named json, json.MarshalIndent with one-space indent, printed with Fprintln to a.stdout. — “asJSON := fs.Bool("json", false, "print state.json")
...
data, _ := json.MarshalIndent(st, "", " ")
		fmt.Fprintln(a.stdout, string(data))”
- FACT-007 `repo-1:internal/run/run.go:43-57` (observation): state.json uses snake_case keys for counters (attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd); token and cost fields are omitempty there. — “ActiveSeconds float64 `json:"active_seconds"`
...InputTokens      int64   `json:"input_tokens,omitempty"`”
- FACT-008 `repo-1:cmd/shogun/stats_test.go:14-78` (observation): TestStatsCountsEveryRunAndFlagsMissingUsage builds five runs (approved with usage, paused without usage, needs_input, failed, and one damaged with invalid state.json), checks the text output and that state.json is unchanged. — “code, out, errs := runCLI(t, ws, "stats")”
- FACT-009 `repo-1:cmd/shogun/main.go:87; README.md:237` (observation): The usage text and the README command table list the flags of stats (`--dir`). — “shogun stats [--dir d]              time, attempts and tokens of every run in .shogun/runs”
- FACT-010 `task:task.md:3` (observation): The text output must be unchanged and the command must not write anything. — “Without the flag the output stays exactly as it is now. The command stays read-only.”
- FACT-011 `task:task.md:3` (observation): The task enumerates the per-run and totals fields of the JSON document. — “one object per run (...) plus the totals (runs by status, damaged count, attempts, active seconds, token sums, cost, and whether the totals are lower bounds)”
- FACT-012 `task:task.md:3` (assumption): Field names are not specified; the plan uses snake_case keys matching state.json (FACT-007), raw active seconds and unrounded cost, and two lower-bound booleans mirroring the two text conditions (FACT-004). Per-run incomplete/unknown counts of the 'not counted' line are derivable from runs[].usage and are not duplicated in totals. — “prints the same data as its table and totals as one JSON document on stdout”
- FACT-013 `task:task.md:3` (assumption): With --json and an empty runs directory, stdout still carries one JSON document (empty runs array, zero totals) so a consumer always gets JSON; see Q-001. — “one JSON document on stdout instead of the table”

## Approach

Add a `json` bool flag to cmdStats. After the runs are collected and sorted, if the flag is set, build a document {runs, totals} in a new helper in stats.go (own summation over the same runStats, reusing runStats.usage and the same lower-bound rules), marshal it with json.MarshalIndent("", " ") like `status --json`, print it with Fprintln to stdout and return; the existing table/totals code path is left untouched. Damaged runs are emitted with a separate small struct (id, status, damaged) so missing counters are absent rather than shown as zeros. Mention the flag in the usage line and the README row. Extend the existing stats test with a JSON run on the same fixture.
- Not chosen: Refactor the text loop into a shared aggregation used by both outputs — Touches the text path that must stay byte-identical; a separate ~15-line summation carries no risk to it.
- Not chosen: One struct per run with omitempty on counters — omitempty would drop legitimate zeros (e.g. attempts 0) from healthy runs; without omitempty damaged runs would show fake zeros.
- Not chosen: A single lower_bound boolean — The text uses two different conditions (attempts/time vs tokens/cost); one flag would lose that data.

## Steps

### S-001 — Add --json to shogun stats

- Objective: Print the stats table and totals as one JSON document when --json is given, leaving the text output unchanged.
- Depends on: none
- Requirements: R-001, R-002, R-003, R-004, R-005; criteria: R-004.C2

Targets:

- repo-1 `cmd/shogun/stats.go` (modify)
- repo-1 `cmd/shogun/main.go` (modify)
- repo-1 `README.md` (modify)

Actions:

1. Before editing anything, capture the current text output: build from HEAD and run `shogun stats --dir <a runs directory with several runs, e.g. a copy of an existing .shogun/runs or one created by the stats test fixture>` into a file outside the repo (e.g. /tmp/stats-before.txt). Use a static copy so no run changes between captures.
2. In cmd/shogun/stats.go add `encoding/json` to the imports and in cmdStats add `asJSON := fs.Bool("json", false, "print runs and totals as JSON")` next to the --dir flag; keep fs.Parse.
3. In cmdStats, after the sort (line 59) and before the `len(runs) == 0` check, add: if *asJSON { return a.printStatsJSON(runs) }. Do not change any other line of the text path.
4. Add in stats.go the JSON types: statsRunJSON {ID `id`, Status `status`, Mode `mode`, Planner `planner`, Reviewer `reviewer`, Attempts `attempts`, ActiveSeconds `active_seconds`, InputTokens `input_tokens`, CacheReadTokens `cache_read_tokens`, CacheWriteTokens `cache_write_tokens`, OutputTokens `output_tokens`, ReasoningTokens `reasoning_tokens`, CostUSD `cost_usd`, Usage `usage`} without omitempty; damagedRunJSON {ID `id`, Status `status` (always "damaged"), Damaged `damaged`}; statsTotalsJSON {ByStatus map[string]int `by_status`, Damaged int `damaged`, Attempts, ActiveSeconds, the five token sums and CostUSD with the same keys as above, SpendLowerBound bool `spend_lower_bound`, UsageLowerBound bool `usage_lower_bound`}; document {Runs []any `runs`, Totals statsTotalsJSON `totals`}. Planner/reviewer are the raw strings (empty when unknown), status is string(r.st.Status).
5. Add `func (a *app) printStatsJSON(runs []runStats) int`: initialise Runs as an empty non-nil slice and ByStatus as an empty map; for each run, damaged (r.st == nil) → count and append damagedRunJSON; otherwise ByStatus[status]++, u := r.usage(), note whether u != "ok", append statsRunJSON from r.st.Counters, and add the counters to the totals exactly as the text loop does (lines 83-90). Set SpendLowerBound = damaged > 0 and UsageLowerBound = anyPartial || damaged > 0. Marshal with json.MarshalIndent(doc, "", " "), Fprintln to a.stdout, return ExitOK. Give it a one-line comment in the file's style.
6. In cmd/shogun/main.go:87 change `shogun stats [--dir d]` to `shogun stats [--dir d] [--json]` keeping the description column aligned as best the line allows; in README.md:237 change the trailing `; `--dir`` to `; `--dir`, `--json` prints the same as JSON`.
7. Rebuild and rerun the same stats command without --json on the same runs directory into /tmp/stats-after.txt and diff against the before capture.

Verification:

- V-001 (command, repo-1): `diff /tmp/stats-before.txt /tmp/stats-after.txt` prints nothing (R-004.C2); `go build ./...` and `gofmt -l cmd/shogun` are clean.

Risks:

- A run in the captured runs directory changes between the before and after captures, producing a false diff. — mitigation: Capture from a static copy of the runs directory, not a live one.

Rollback: Revert the changes to cmd/shogun/stats.go, cmd/shogun/main.go and README.md (git checkout of those files); nothing else is touched and no data is written.

### S-002 — Test stats --json on the existing fixture

- Objective: Prove the JSON document content, the unchanged text output and read-only behaviour.
- Depends on: S-001
- Requirements: R-001, R-002, R-003, R-004, R-005; criteria: R-001.C1, R-002.C1, R-002.C2, R-003.C1, R-003.C2, R-004.C1, R-005.C1

Targets:

- repo-1 `cmd/shogun/stats_test.go` (modify)

Actions:

1. In TestStatsCountsEveryRunAndFlagsMissingUsage, after the existing text assertions and the state.json comparison, leave all existing assertions as they are and add: `code, out, errs = runCLI(t, ws, "stats", "--json")`; require ExitOK and empty errs.
2. Decode stdout with a json.Decoder into a struct mirroring the document (runs as []map[string]any, totals as a typed struct or map); require a second Decode to return io.EOF; require out not to contain "RUN" nor "total:".
3. Assert 5 runs in id order; the approved run's fields and the paused run's usage "unknown" per R-002.C1; the damaged run 20260927-000003-c has status "damaged", non-empty `damaged` and no `attempts` key; non-damaged runs have no `damaged` key.
4. Assert totals per R-003.C1 and both lower-bound booleans true per R-003.C2.
5. Re-read the approved run's state.json after the JSON run and compare it with `before`; add `encoding/json` and `io` imports.

Verification:

- V-002 (test, repo-1): `go test ./cmd/shogun -run TestStats` passes, covering R-001.C1, R-002.C1, R-002.C2, R-003.C1, R-003.C2 and R-005.C1.
- V-003 (inspect, repo-1): `git diff cmd/shogun/stats_test.go` shows only added lines; the existing text assertions are unmodified and pass (R-004.C1).
- V-004 (test, repo-1): `go test ./...` passes.

Rollback: Revert cmd/shogun/stats_test.go; test-only change.

## End-to-end verification

- R-004.C1: The existing TestStatsCountsEveryRunAndFlagsMissingUsage text assertions pass without modification.
- R-004.C2: `shogun stats --dir <same runs dir>` output captured before and after the change is byte-identical (diff is empty).
- R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before.

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C2 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-003 | R-003.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-003 | R-003.C2 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-004 | R-004.C1 | S-002, end-to-end | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-004 | R-004.C2 | S-001, end-to-end | S-001/V-001 |
| R-005 | R-005.C1 | S-002, end-to-end | S-002/V-002, S-002/V-003, S-002/V-004 |

## Review history


Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
