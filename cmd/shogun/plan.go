package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/run"
)

// cmdPlan performs intake (validates config, creates the run dir, snapshots repos and inputs,
// writes task/config/manifest/state) and then runs the planning stages.
func (a *app) cmdPlan(args []string) int {
	fs := a.newFlagSet("plan")
	var repos, ins multiFlag
	taskFile := fs.String("task-file", "", "read the task text from this file")
	fs.Var(&repos, "repo", "repository root to study (repeatable; default: current directory)")
	fs.Var(&ins, "input", "file path or http(s) URL that must be studied (repeatable)")
	planner := fs.String("planner", "", "override planner model spec (claude/<model>:<effort>)")
	reviewer := fs.String("reviewer", "", "override reviewer model spec (codex/<model>:<effort>)")
	out := fs.String("out", "", "path of the primary plan file (default: plans_dir/<project>/<run-id>.md or docs/plans/<run-id>.md)")
	project := fs.String("project", "", "project name for the library layout")
	lang := fs.String("lang", "", "plan language (default: the task's language)")
	auto := fs.Bool("auto", false, "never wait for a terminal; material unknowns produce needs_input")
	thorough := fs.Bool("thorough", false, "staged pipeline (research → outline → one step at a time → final review) for very large plans; slower and far more expensive than the default")
	asJSON := fs.Bool("json", false, "print the final result as JSON on stdout")
	maxCalls := fs.Int("max-calls", 0, "cap on physical model attempts (0 = derived)")
	maxTime := fs.Duration("max-time", 0, "cap on active time (0 = derived)")
	positionals, err := parseArgs(fs, args)
	if err != nil {
		return ExitError
	}
	task, err := a.readTask(positionals, *taskFile)
	if err != nil {
		return a.errorf("%v", err)
	}
	loaded, err := config.Load(a.cwd, config.Overrides{Planner: *planner, Reviewer: *reviewer, Project: *project, Lang: *lang, MaxCalls: *maxCalls, MaxTime: *maxTime}, a.getenv)
	if err != nil {
		return a.errorf("%v", err)
	}
	cfg := loaded.Config
	if len(repos) == 0 {
		repos = multiFlag{a.cwd}
	}
	now := a.now()
	id := run.NewID(now, task, nil)
	r, err := run.Create(run.RunsRoot(a.cwd), id)
	if err != nil {
		return a.errorf("%v", err)
	}
	if err := r.Lock(); err != nil {
		return a.errorf("%v", err)
	}
	defer r.Unlock()
	st := run.NewState(id, now)
	// Fast path (default): one planner and one reviewer attempt, at most one more pair — four physical
	// attempts in total, retries and format corrections included — within ten minutes of active
	// time (host work included, waiting for the user excluded) unless --max-calls/--max-time say
	// otherwise.
	st.Limits = run.Limits{MaxLogicalCalls: 2 * pipeline.FastRounds, MaxAttempts: 2 * pipeline.FastRounds,
		MaxActiveSeconds: (10 * time.Minute).Seconds(), CallDeadlineSecs: cfg.CallDeadline.Seconds(), Source: "fast"}
	if *thorough {
		st.Mode = pipeline.ModeThorough
		st.Limits = run.Limits{MaxLogicalCalls: 4 * cfg.ReviewRounds, MaxAttempts: 3 * 4 * cfg.ReviewRounds,
			CallDeadlineSecs: cfg.CallDeadline.Seconds(), Source: "pre-outline"}
	}
	if cfg.MaxCalls > 0 {
		st.Limits.MaxAttempts, st.Limits.Source, st.Limits.ExplicitAttempts = cfg.MaxCalls, "flag", true
	}
	if cfg.MaxTime > 0 {
		st.Limits.MaxActiveSeconds = cfg.MaxTime.Seconds()
	}
	fail := func(code int, reason string, err error) int {
		st.Status, st.Reason = run.StatusFailed, reason
		if code == ExitNeedsInput {
			st.Status = run.StatusNeedsInput
		}
		_ = r.SaveState(st, a.now())
		if err != nil {
			fmt.Fprintf(a.stderr, "error: %s: %v\n", reason, err)
		}
		fmt.Fprintf(a.stderr, "run %s → %s (%s)\n", id, st.Status, r.Dir)
		return code
	}
	if err := r.WriteArtifact("task.md", []byte(task)); err != nil {
		return fail(ExitError, "write task", err)
	}
	snap, err := loaded.Snapshot()
	if err != nil {
		return fail(ExitError, "render config snapshot", err)
	}
	if err := r.WriteArtifact("config.snapshot.toml", snap); err != nil {
		return fail(ExitError, "write config snapshot", err)
	}
	fmt.Fprintf(a.stderr, "[intake] run %s\n", id)
	// Intake runs under the run's active-time allowance: repository scans and input downloads are
	// cancelled at the deadline instead of being counted after the fact.
	ictx, cancel := a.ctx, func() {}
	if st.Limits.MaxActiveSeconds > 0 {
		ictx, cancel = context.WithDeadline(a.ctx, now.Add(time.Duration(st.Limits.MaxActiveSeconds*float64(time.Second))))
	}
	defer cancel()
	overrun := func() int {
		st.Counters.ActiveSeconds += a.now().Sub(now).Seconds()
		st.Status, st.Reason = run.StatusPaused, fmt.Sprintf("limit: the %.0fs active-time allowance ran out during intake", st.Limits.MaxActiveSeconds)
		_ = r.SaveState(st, a.now())
		fmt.Fprintf(a.stderr, "run %s → paused: %s (%s)\n", id, st.Reason, r.Dir)
		return ExitLimit
	}
	st.Publish.Path = outputPath(*out, cfg, a.cwd, id)
	man := &inputs.Manifest{Version: inputs.ManifestVersion, CreatedAt: now.UTC().Format(time.RFC3339), Workspace: a.cwd,
		Exclude: []string{st.Publish.Path, library.ReceiptPath(st.Publish.Path)}}
	for i, root := range repos {
		rp, err := inputs.RepoManifest(ictx, fmt.Sprintf("repo-%d", i+1), root, man.Exclude...)
		if ictx.Err() != nil {
			return overrun()
		}
		if err != nil {
			return fail(ExitError, "repository "+root, err)
		}
		man.Repos = append(man.Repos, rp)
		fmt.Fprintf(a.stderr, "[intake] %s %s git=%v head=%.12s\n", rp.ID, rp.Root, rp.IsGit, rp.Head)
	}
	srcs, inErr := inputs.NewMaterializer().Materialize(ictx, r.Dir, a.cwd, ins)
	if ictx.Err() != nil {
		return overrun()
	}
	man.Inputs = srcs
	for _, s := range srcs {
		if s.Status == "ok" {
			fmt.Fprintf(a.stderr, "[intake] %s %s → %s (%d bytes)\n", s.ID, s.Origin, s.StoredPath, s.Size)
		} else {
			fmt.Fprintf(a.stderr, "[intake] %s %s → ERROR %s\n", s.ID, s.Origin, s.Error)
		}
	}
	man.ComputeFingerprint()
	if err := man.Save(filepath.Join(r.Dir, "manifest.json")); err != nil {
		return fail(ExitError, "write manifest", err)
	}
	st.Hashes["manifest"] = man.Fingerprint
	if inErr != nil {
		if errors.Is(inErr, inputs.ErrUnavailable) {
			return fail(ExitNeedsInput, "explicit inputs are unavailable; export them as files and re-run", nil)
		}
		return fail(ExitError, "inputs", inErr)
	}
	st.Cursor = run.Cursor{Stage: pipeline.StagePlan}
	if st.Mode == pipeline.ModeThorough {
		st.Cursor = run.Cursor{Stage: pipeline.StageResearch}
	}
	st.Status = run.StatusRunning
	if err := r.SaveState(st, a.now()); err != nil {
		return a.errorf("%v", err)
	}
	st.Counters.ActiveSeconds += a.now().Sub(now).Seconds() // intake is part of the run's active time
	fmt.Fprintf(a.stderr, "[intake] complete: %s\n", r.Dir)
	return a.runPipeline(r, st, cfg, *auto || !a.interactive, *asJSON)
}

// readTask returns the task text from exactly one source: positional text or --task-file.
func (a *app) readTask(positional []string, file string) (string, error) {
	text := strings.TrimSpace(strings.Join(positional, " "))
	switch {
	case text != "" && file != "":
		return "", errors.New("give the task either as text or with --task-file, not both")
	case text == "" && file == "":
		return "", errors.New("task is required: shogun plan \"<task>\" or --task-file <file>")
	case file != "":
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(a.cwd, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("%s is empty", file)
		}
		return string(data), nil
	}
	return text, nil
}
