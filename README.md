# Shogun

Shogun turns a task into a reviewed, minimal implementation plan in a few minutes. A planner model (Anthropic, through `claude -p`) writes the whole plan, and an independent reviewer model (OpenAI, through `codex exec`) checks it. Shogun keeps the state, checks the contracts mechanically and decides when a stage may pass. The result is one self-contained Markdown plan with a verifiable approval receipt.

Shogun plans; it never executes the plan and never changes the repositories it studies.

```
intake → plan (1 planner call) → review of the rendered plan (1 call) → [one revision + one review] → publish
```

A typical plan takes 2–4 model calls. `--thorough` switches to the staged pipeline (research → outline → one step at a time → final review) for very large plans; it is much slower and far more expensive.

## What a plan contains

A published plan has these parts:

- the task verbatim;
- the requirements registry with testable criteria, plus the constraints and non-goals;
- the inputs and their versions;
- the decisions and assumptions, with where each came from;
- facts with `source:location`;
- the approach and the alternatives rejected;
- the steps: targets, concrete actions, verifications, risks and rollback;
- the end-to-end checks;
- a `requirement → criterion → steps → verification` table;
- a short review history.

Plans are **minimal by design**. They do the task and its basic functionality: no side refactors, no "while we are here" improvements, no verification heavier than the change needs. Every step must serve an accepted criterion. The reviewer asks to remove anything the result does not need. An assumption taken to fill a gap never becomes a requirement.

After the approved body, an **Execution log** table has one row per step. The executor updates it; it is outside the approval.

## Requirements

- macOS (the read-only sandboxes are verified there; Linux builds and passes the tests, but its sandbox behaviour is not certified).
- Go 1.26 to build.
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude`), logged in with a subscription.
- [Codex CLI](https://github.com/openai/codex) (`codex`), logged in with a ChatGPT account.
- No API keys. Shogun strips `ANTHROPIC_*`, `OPENAI_*` and `CODEX_API_KEY` from the child environment, so the subscriptions are used. Calls consume your subscription limits.

## Install

```sh
go install github.com/killabayte/shogun/cmd/shogun@latest
```

## Set up

Shogun needs the real executables. A shell alias or function is not visible to it. If `codex` is not on `PATH`, point to the binary in a config file:

```toml
# ~/.config/shogun/config.toml  (global)  or  <workspace>/.shogun/config.toml  (per workspace)
codex_command = "/Applications/ChatGPT.app/Contents/Resources/codex"
plans_dir     = "~/plans"             # optional plan library; default: <workspace>/docs/plans
planner       = "claude/opus:high"    # default
reviewer      = "codex/gpt-6-astra:high"  # default; the reviewer's effort is never below high
```

`shogun config` prints every effective value and where it came from.

Then certify the configuration once. Repeat this whenever a CLI, a model or the config changes:

```sh
shogun doctor          # offline: binaries, versions, feature names, directories, preflight status
shogun doctor --live   # one short control call per model in a disposable fixture
```

`--live` makes exactly one physical attempt per model, within five minutes in total; it prints its spend and does not start the second model once that time is used. It checks isolation on the real CLIs:

- both models can read three roots;
- neither can write (the check needs a recorded sandbox denial, not the model's word);
- no model can delegate to sub-agents;
- the requested model is the one actually used, for both CLIs; the requested reasoning effort is confirmed for Codex, which reports it per call. Claude does not report effort, so it stays `unknown`.

The record goes to `.shogun/preflight/`, one file per configuration. `shogun plan` refuses to call any model without a certificate for the exact configuration it would use.

To use another model pair, set `planner`/`reviewer` in the config, or pass them per run and certify that pair once:

```sh
shogun doctor --live --planner claude/opus:xhigh --reviewer codex/gpt-6-sol:high
shogun plan --planner claude/opus:xhigh --reviewer codex/gpt-6-sol:high "…"
```

A running plan keeps the models it started with: `resume` always uses the run's saved configuration.

## Plan

```sh
shogun plan "Add a --version flag that prints the version and exits"

shogun plan --task-file task.md \
  --repo ../api-gateway --repo ../connexa-plans \
  --input docs/rfc.md --input https://example.org/spec \
  --out docs/plans/rate-limit.md
```

- **Where things run.** The current directory is the Shogun workspace; each run lives in `.shogun/runs/<run-id>/`. Put flags before the task text.
- **Repositories.** `--repo` is repeatable and defaults to the current directory. Shogun snapshots each one: HEAD, the content of local changes and of untracked files.
- **Inputs.** `--input` (a file or an http(s) URL) is repeatable and mandatory to study. Each input is stored as a snapshot with its SHA-256.
- **Output.** On success Shogun prints the plan path and exits with 0. The plan goes to `--out`, else `plans_dir/<project>/<run-id>.md`, else `docs/plans/<run-id>.md`. The approval receipt `<plan>.approval.json` sits next to it.
- **Language.** The plan is written in the task's language; `--lang` overrides this.

### Questions

The models ask only about material unknowns, such as a public API choice, data loss, contradicting inputs or a missing specification.

- **In a terminal:** the questions are shown with a recommendation. Press Enter to accept it, type a number to pick an option, or type an answer. There are at most two rounds of questions.
- **With `--auto`, or without a terminal:** a non-blocking question proceeds on its stated, reversible assumption, which is recorded. A blocking question stops the run with exit 3 and writes `questions.json`. Answer it and resume:

```json
{"schema_version": 1, "answers": [{"question_id": "Q-001", "answer": "semver", "files": ["/path/to/versioning.md"]}]}
```

```sh
shogun resume <run-id> --answers .shogun/runs/<run-id>/answers.json
```

Keep `answers.json` in the run directory. A new file inside a studied repository is itself a change to that repository's inputs. Files listed in `files` become snapshot inputs.

## Resume, limits, drift

- `shogun resume <run-id>` continues from the last checkpoint with the run's own saved configuration. If Shogun crashed after a call finished but before the checkpoint, that result is recovered without calling the model again.
- **Budgets.** The default plan has **four physical model attempts in total**: one planner and one reviewer attempt, and at most one more pair for a revision. Transport retries, format corrections and question-driven re-planning all count against the four. It also has **ten minutes of active time**, which includes Shogun's own work and excludes time spent waiting for your answers. A third of that time is reserved for the review. When a limit is reached, the run pauses and keeps the last draft, clearly marked as not approved. Only blocker and major findings block; minor ones become review notes. After every call Shogun prints the spend so far: calls and attempts against the limits, active and waiting time, and tokens (uncached input, cache read, cache write, output, reasoning). It also prints Claude's reported list-price equivalent, which is not a subscription charge. Missing telemetry is shown as incomplete, never as zero. These are limits checked between calls: a single call cannot be interrupted mid-way on tokens, only on time. With `--thorough`, each unit — research, outline, each step, the final review — gets up to `review_rounds` (6) rounds. At the first approved outline the ceiling is fixed at `2·R·(ceil(N/detail_batch)+3)` calls for that generation. `--max-calls N` (physical attempts) and `--max-time D` (active time) cap a run explicitly; they are recorded and kept on `resume`.
- **Drift.** If a studied repository changes during a run, Shogun refuses to continue or to publish. `shogun resume <run-id> --refresh` starts a new generation: new snapshots, a new research, no inherited approvals, spend so far kept.
- `shogun status <run-id>` shows the stage, counters, limits and the reason for the last stop.

Exit codes: `0` plan approved and published · `1` limit, stalemate, rate limit or drift pause · `2` configuration, tool or protocol error · `3` needs input · `130` interrupted.

## Verify and the library

```sh
shogun verify docs/plans/<run-id>.md
shogun list --dir ~/plans --status planned
```

`verify` compares the approved body (the bytes between the markers) and the immutable frontmatter with the receipt. Results:

- `valid` (0);
- `changed` (1);
- `unverifiable` (2): the receipt is missing or corrupt;
- `invalid_format` (2): for example, duplicate markers or duplicate YAML keys.

Editing `status`, `tags`, `updated` or the Execution log does not break verification. `verify` works on a plan moved elsewhere together with its receipt; the run directory is not needed.

`verify` does **not** check that the work was done, that the repositories are still current, or that the plan is correct. A receipt is a record of what was approved, not a signature.

`plans_dir` is an ordinary folder of Markdown files. It can be opened as an Obsidian vault and synced with Git or any single sync tool. Shogun never commits, pushes or copies plans.

## Development

```sh
go vet ./... && go test -race ./... && go build ./cmd/shogun
```

The test suite never calls a model. The adapters are tested against a fake CLI (the test binary itself) and against streams recorded from the real CLIs (`internal/provider/testdata/`). Live checks are opt-in: `shogun doctor --live`, or a real `shogun plan`.

Design, decisions and the review history of every stage: [`docs/plans/shogun-v1.md`](docs/plans/shogun-v1.md), [`docs/cli-compatibility.md`](docs/cli-compatibility.md), [`docs/reviews/`](docs/reviews/).
