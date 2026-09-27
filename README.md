# shogun

[![build](https://github.com/killabayte/shogun/actions/workflows/ci.yml/badge.svg)](https://github.com/killabayte/shogun/actions/workflows/ci.yml)

shogun turns a task into a reviewed, minimal implementation plan. It drives `claude -p` as the planner and
`codex exec` as an independent reviewer, both as supervised subprocesses. The result is one Markdown plan
with a verifiable approval receipt.

**It plans and does nothing else.** It never executes the plan and never modifies the repositories it
studies: both models run read-only, the reviewer in a sandbox that refuses writes, and shogun checks this
before it calls them. What it produces is a file an executor — a person, Claude Code, any agent — can
follow without the conversation that led to it.

**A plan is small on purpose.** It covers the task and its basic functionality. It has no side refactors,
no "while we are here" improvements, and no verification heavier than the change needs. Every step serves an
accepted requirement. The reviewer asks to remove anything the result does not need, and an assumption made
to fill a gap never becomes a requirement.

**It is fast and bounded.** By default one planner call writes the whole plan and one reviewer call checks
it; a revision is at most one more pair. A run is capped at four physical model attempts and ten minutes of
active time, both enforced, and it prints what it has spent after every call.

## Why

**Two different models, not one model twice.** The planner is Anthropic, the reviewer is OpenAI, and neither
can swap roles. The reviewer checks the plan against the original task and inputs rather than the planner's
summary of them, so an omitted requirement is caught by something that did not omit it.

**Mechanical gates, not a vibe check.** A reviewer's `approve` is necessary but not sufficient. shogun itself
checks that every mandatory criterion is assigned to a step and proven by a verification, that the
dependencies have no cycles, that every repository and input was studied, and that the reviewer's coverage
table matches the real assignments. A plan with a gap does not pass, whatever the verdict says.

**The approval is bound to bytes.** The published plan has an approval receipt next to it. `shogun verify`
tells you if the approved body or its metadata changed since, even after the file was moved elsewhere. The
execution log and status fields stay editable, so the plan can track its own execution.

**Questions instead of guesses.** Material unknowns — a public API choice, data loss, contradicting inputs,
a missing specification — become questions for you. In a terminal they are asked with a recommendation. With
`--auto` a blocking one stops the run with its questions saved, and `resume --answers` continues it.

**Every spent token is visible.** Each call records its model, effort, attempts, time and reported usage;
cache reads and writes are kept apart, and missing telemetry is shown as incomplete rather than zero. Runs
survive crashes: a finished result is recovered without calling the model again.

## Install

With a Go toolchain (1.26 or later):

```
go install github.com/killabayte/shogun/cmd/shogun@latest
```

Or build from a clone:

```
git clone https://github.com/killabayte/shogun.git && cd shogun
make build        # produces .bin/shogun
make install      # and symlinks it to /usr/local/bin/shogun
```

`make install` links rather than copies, so a later `make build` is picked up without reinstalling. Override
the location with `BINDIR` when `/usr/local/bin` is not writable. `make uninstall` removes the link.

shogun drives the model CLIs, so both must be installed and logged in:
[Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude`) with a subscription and the
[Codex CLI](https://github.com/openai/codex) (`codex`) with a ChatGPT account. No API keys are used:
`ANTHROPIC_*`, `OPENAI_*` and `CODEX_API_KEY` are stripped from the child environment, so the calls run on
your subscriptions and count against their limits.

The read-only sandboxes are verified on macOS. shogun builds and passes its tests on Linux, but sandbox
behaviour there is not certified.

## Quick start

shogun needs the real executables; a shell alias or function is not visible to it. If `codex` is not on
`PATH`, name it in the config:

```toml
# ~/.config/shogun/config.toml, or <workspace>/.shogun/config.toml
codex_command = "/Applications/ChatGPT.app/Contents/Resources/codex"
```

Certify the configuration once, and again whenever a CLI, the models or the config change:

```console
$ shogun doctor --live
live preflight: one control call per model (traces in /var/folders/…/shogun-preflight-3985772635)
limits: 1 physical attempt per model, 5m0s in total
pass       claude/no-write     fixture unchanged, PROBE_WRITE.txt absent
pass       claude/call         reported {Model:claude-opus-5-5 Effort:unknown ApprovalPolicy:dontAsk PermissionProfile:unknown}
pass       claude/reads        all three random codewords returned
pass       codex /no-write     fixture unchanged, PROBE_WRITE.txt absent
pass       codex /call         reported {Model:gpt-6-astra Effort:high ApprovalPolicy:Never …}
pass       codex /reads        all three random codewords returned
pass       codex /write-denied apply_patch on the probe file rejected by the read-only sandbox (router error)
preflight: certified, recorded in .shogun/preflight/15b1ce2fa61aa58a.json
spend: 2/2 call(s), 2/2 attempt(s), 0.6/5 min active (+0.0 min waiting for you); tokens: 9682 input, 22542 cache read, 5608 cache write, 1150 output, 0 reasoning; Claude reported list-price equivalent $0.06 (not a subscription charge)
```

Then plan. The current directory is the workspace; `--repo` and `--input` are repeatable, and flags go before
the task text:

```
shogun plan "Add a --version flag that prints the version and exits"

shogun plan --task-file task.md \
  --repo ../api-gateway --repo ../connexa-plans \
  --input docs/rfc.md --input https://example.org/spec
```

On success shogun prints the path of the published plan and exits 0. Progress, the spend after each call and
the reason for any stop go to stderr.

## How it works

1. **intake.** The task is stored verbatim. Each repository is snapshotted: HEAD, plus the content of local
   changes and untracked files. Each input is saved with its SHA-256. The effective configuration is frozen
   for the run.
2. **plan.** One planner call returns the whole plan as structured output: facts with their source locations,
   the requirements registry with testable criteria, the approach and the rejected alternatives, and steps
   with targets, actions, one verification per criterion, and a rollback.
3. **check.** shogun applies its contracts: sources covered and cited correctly, every mandatory criterion
   assigned, no dependency cycles, targets in known repositories. A failure goes back to the planner without
   spending a review.
4. **review.** shogun renders the candidate document, and one reviewer call reviews exactly those bytes. Only
   blocker and major findings block; minor ones are kept as review notes. On `revise`, one planner revision
   and one more review follow, and that is the limit.
5. **publish.** Repositories and input snapshots are checked for drift first. Then the receipt is written, then
   the plan, each atomically and never over a file that is not shogun's. The run is `approved` only once
   `verify` passes.

**Budgets are enforced, not advisory.** The whole run is capped at four physical attempts — retries, format
corrections and re-planning after answers included — and ten minutes of active time, which counts shogun's
own work and excludes time spent waiting for you. A third of the time is kept for the review. When a limit is
hit, the run pauses with the last draft clearly marked as not approved. `resume --max-calls` or `--max-time`
raise a limit explicitly. Token totals are measured after each attempt, so tokens cannot be capped inside a
single call; time can.

**`--thorough` is the heavy mode.** It runs a staged pipeline instead: research, outline, then one step at a
time, then a final review. Each unit has its own review rounds. It is meant for very large plans and is much
slower and more expensive than the default.

## Runs

Every run is a directory under the workspace. It holds everything needed to resume the run or audit it
afterwards:

```
.shogun/runs/<run-id>/
├── task.md  manifest.json  config.snapshot.toml  state.json
├── inputs/                    snapshots of --input files and URLs
├── calls/0001-plan-planner/   prompt.md, request.json, argv, streams and result of every attempt
├── plan/1.json  plan/1.md     each planner revision and the candidate rendered from it
├── reviews/plan-1.json        each review
├── decisions.json             answers and recorded assumptions
├── candidate.md  approval.json  PLAN.md
└── review-notes.md            minor findings left open by the approving review
```

`shogun status <run-id>` shows the stage, the reason for the last stop, and consumption against the limits.
`shogun resume <run-id>` continues from the last checkpoint on the run's own saved configuration; the models
of a running plan do not change. If a studied repository changed meanwhile, shogun refuses to continue or to
publish until `resume --refresh` starts a new generation. The new generation plans again on fresh snapshots
and keeps the spend.

## Output

The plan is Markdown with YAML frontmatter, then exactly one pair of markers around the approved body, then
an execution log:

```markdown
---
title: Add a --version flag that prints the version and exits
plan_id: 20260926-100000-version-flag-a1b2
revision: 1
status: planned            # planned | in_progress | blocked | done | dropped
planner: claude/opus:high
reviewer: codex/gpt-6-astra:high
…
---

<!-- shogun:plan:begin -->
# Add a --version flag that prints the version and exits
## Goal and success criteria · Scope and non-goals · Inputs and versions · Requirements
## Decisions and assumptions · Context · Approach · Steps · End-to-end verification
## Traceability · Review history
<!-- shogun:plan:end -->

## Execution log

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
```

`<plan>.approval.json` next to it records the body hash, the immutable metadata, the manifest digest and the
requested and reported models. It holds no local paths or prompts. Editing `status`, `tags`, `updated` or the
execution log keeps the plan valid; anything else does not.

| Code | `plan` / `resume` |
|---|---|
| `0` | plan approved and published |
| `1` | paused: a limit, a stalemate, a subscription rate limit, or drift; `resume` continues it |
| `2` | configuration, tool or protocol error |
| `3` | needs input: answer `questions.json` and `resume --answers` |
| `130` | interrupted |

## Models

The planner is `claude/<model>:<effort>` and the reviewer is `codex/<model>:<effort>`. The defaults are
`claude/opus:high` and `codex/gpt-6-astra:high`; the reviewer's effort is never below `high`. Model ids are
opaque strings, so a new model needs no new shogun release.

Set them in the config, or per run, and certify the pair you use:

```
shogun doctor --live --planner claude/opus:xhigh --reviewer codex/gpt-6-sol:high
shogun plan --planner claude/opus:xhigh --reviewer codex/gpt-6-sol:high "…"
```

The adapters confirm on every call that the requested model answered, and for Codex the requested effort
too. Claude does not report effort, so it is recorded as `unknown`.

## Subcommands

| command | does |
|---|---|
| `shogun plan` | runs intake, plans, reviews and publishes |
| `shogun resume <run>` | continues a run: `--answers`, `--refresh`, `--max-calls`, `--max-time` |
| `shogun status <run>` | stage, stop reason, spend and limits; `--json` prints the state |
| `shogun verify <plan.md>` | `valid` (0), `changed` (1), `unverifiable` (2, receipt missing) or `invalid_format` (2) |
| `shogun stats` | every run in `.shogun/runs` (stopped and failed too): status, models, attempts, active time, tokens, Claude list-price equivalent; runs without usage are marked `unknown`, unreadable ones `damaged`; `--dir` |
| `shogun list` | published plans with execution status and integrity; `--dir`, `--status`, `--project` |
| `shogun doctor` | binaries, versions, config and the preflight certificate; `--live` certifies a model pair |
| `shogun config` | every effective setting and the layer it came from |

`verify` does not check that the work was done, that the repositories are still current, or that the plan
is right. A receipt records what was approved; it is not a signature.

A plans directory (`plans_dir`) is an ordinary folder of Markdown files. It can be opened as an Obsidian
vault and synced with Git or any single sync tool; shogun never commits, pushes or copies plans.

## Configuration

`~/.config/shogun/config.toml` is global and `<workspace>/.shogun/config.toml` is per workspace; flags win
over both.

| key | default | meaning |
|---|---|---|
| `planner`, `reviewer` | `claude/opus:high`, `codex/gpt-6-astra:high` | the model pair |
| `claude_command`, `codex_command` | `claude`, `codex` | executables, when not on `PATH` |
| `plans_dir` | none (plans go to `docs/plans/`) | the plan library |
| `project`, `lang` | workspace name, the task's language | library folder and plan language |
| `max_calls`, `max_time` | unset: 4 attempts and 10 min on the default path | the run's limits (physical attempts, active time) |
| `call_deadline` | 30 min | one call's limit, always within the run's remaining time |
| `max_context_tokens` | 400000 | estimated prompt size above which a call is not started (both modes) |
| `review_rounds`, `detail_batch` | 6, 1 | `--thorough` only |

## Development

```
make build    # build .bin/shogun with the git revision baked in
make install  # symlink .bin/shogun into $BINDIR (default /usr/local/bin)
make test     # race detector plus coverage
make race     # race detector only
make lint     # go vet and gofmt, plus golangci-lint when installed
make fmt      # gofmt, plus goimports when installed
```

No test calls a model. The adapters run against a fake CLI (the test binary itself) and against streams
recorded from the real CLIs in `internal/provider/testdata/`. The pipeline runs against scripted runners.
Live checks are opt-in: `shogun doctor --live`, or a real `shogun plan`.

## License

MIT
