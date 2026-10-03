---
title: Add a --version flag that prints the version and exits
plan_id: 20260926-100000-version-flag-a1b2
revision: 1
created: "2026-09-26"
updated: "2026-09-26"
status: planned
run_id: 20260926-100000-version-flag-a1b2
project: test
repos:
    - demo
tags:
    - shogun
    - plan
planner: claude/fable:xhigh
reviewer: codex/gpt-6-astra:xhigh
---

<!-- shogun:plan:begin -->

# Add a --version flag that prints the version and exits

## Goal and success criteria

Task, verbatim:

> Add a --version flag that prints the version and exits

- **R-001** statement R-001
  - R-001.C1: criterion R-001.C1

## Scope and non-goals

- **R-002** (constraint) statement R-002
  - R-002.C1: criterion R-002.C1

## Inputs and versions

- repo-1: `demo` at `9096172da7ad`, clean

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | statement R-001 | R-001.C1: criterion R-001.C1 | task |
| R-002 | constraint | true | statement R-002 | R-002.C1: criterion R-002.C1 | task |

## Decisions and assumptions

- Q-001 (user): Version source? → a constant

## Context

- FACT-001 `repo-1:main.go:1` (observation): a Go program — “package main”

## Approach

s

## Steps

### S-001 — t

- Objective: o
- Depends on: none
- Requirements: R-001; criteria: R-001.C1

Targets:

- repo-1 `main.go` (modify)

Actions:

1. edit main.go

Verification:

- V-001 (command, repo-1): prints the version

Rollback: revert the commit

### S-002 — t

- Objective: o
- Depends on: none
- Requirements: R-002; criteria: R-002.C1

Targets:

- repo-1 `main.go` (modify)

Actions:

1. edit main.go

Verification:

- V-001 (command, repo-1): prints the version

Rollback: revert the commit

## End-to-end verification

Every criterion is verified inside its step (see the traceability table).

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-001 | S-001/V-001 |
| R-002 | R-002.C1 | S-002 | S-002/V-001 |

## Review history

- detail:S-001: 1 review round(s), 0 finding(s)
- detail:S-002: 1 review round(s), 0 finding(s)
- outline: 1 review round(s), 0 finding(s)
- research: 2 review round(s), 0 finding(s)

Planner claude/fable:xhigh, reviewer codex/gpt-6-astra:xhigh. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
