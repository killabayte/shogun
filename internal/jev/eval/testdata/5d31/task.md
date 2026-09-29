Add a `--json` flag to `shogun stats`.

With `--json`, `shogun stats` prints the same data as its table and totals as one JSON document on stdout instead of the table: one object per run (id, status, mode, planner, reviewer, attempts, active seconds, input / cache read / cache write / output / reasoning tokens, Claude list-price equivalent, usage state, and the damage reason for a damaged run) plus the totals (runs by status, damaged count, attempts, active seconds, token sums, cost, and whether the totals are lower bounds). Without the flag the output stays exactly as it is now. The command stays read-only.

Every object in `runs` has all the listed fields, damaged runs included: values that are not available for a damaged run are `null`, not zero, and `damaged` holds the damage reason. The test checks that these keys are present and `null` for the damaged run.

With `--json` and no runs in the directory, stdout still holds one JSON document: `runs` is `[]`, every numeric total is `0`, `by_status` is `{}`, both lower-bound flags are `false`, and the exit code is 0. The early exit with a note on stderr stays for the text mode only. A test covers the empty directory.

The earlier approved plan is given as reference material: keep what it gets right and correct the empty-directory case; where it differs from this task, the task governs.
