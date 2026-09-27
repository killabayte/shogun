package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

func (a *app) cmdResume(args []string) int {
	fs := a.newFlagSet("resume")
	answers := fs.String("answers", "", "answers.json for a needs_input run")
	refresh := fs.Bool("refresh", false, "start a new generation after input drift")
	maxCalls := fs.Int("max-calls", 0, "raise the attempt cap")
	maxTime := fs.Duration("max-time", 0, "raise the active-time cap")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitError
	}
	if len(pos) != 1 {
		return a.errorf("usage: shogun resume <run-id|dir> [--answers f] [--refresh] [--max-calls N] [--max-time D]")
	}
	dir, err := run.Resolve(a.cwd, pos[0])
	if err != nil {
		return a.errorf("%v", err)
	}
	r, err := run.Open(dir)
	if err != nil {
		return a.errorf("%v", err)
	}
	if err := r.Lock(); err != nil {
		return a.errorf("%v", err)
	}
	defer r.Unlock()
	st, err := r.LoadState()
	if err != nil {
		return a.errorf("%v", err)
	}
	if st.Status == run.StatusApproved && st.Publish.Done {
		fmt.Fprintf(a.stderr, "[resume] run %s is already approved and published\n", st.RunID)
		fmt.Fprintln(a.stdout, st.Publish.Path)
		return ExitOK
	}
	var ans *schema.Answers
	if *answers != "" {
		data, err := os.ReadFile(*answers)
		if err != nil {
			return a.errorf("%v", err)
		}
		if ans, err = schema.Parse[schema.Answers](schema.KindAnswers, data); err != nil {
			return a.errorf("%s: %v", *answers, err)
		}
		fmt.Fprintf(a.stderr, "[resume] answers file is valid (%s)\n", *answers)
	}
	cfg, err := config.LoadSnapshot(filepath.Join(dir, "config.snapshot.toml")) // the run's own config (§9)
	if err != nil {
		return a.errorf("%v", err)
	}
	// Raising a limit is an explicit, recorded decision; it never lowers spend counters. It applies
	// before anything else, so the preparation below already runs under the raised allowance.
	if *maxCalls > 0 {
		st.Limits.MaxAttempts, st.Limits.ExplicitAttempts = *maxCalls, true // the reserve's provenance is unchanged
	}
	if *maxTime > 0 {
		st.Limits.MaxActiveSeconds = maxTime.Seconds()
	}
	limitStop := func() int {
		st.Status, st.Reason = run.StatusPaused, "limit: the active-time allowance ran out while preparing the resume; raise it with --max-time"
		_ = r.SaveState(st, a.now())
		fmt.Fprintf(a.stderr, "run %s → paused: %s\n", st.RunID, st.Reason)
		return ExitLimit
	}
	if l := st.Limits.MaxActiveSeconds; l > 0 && st.Counters.ActiveSeconds >= l {
		return limitStop()
	}
	// Resume preparation (re-snapshotting, the drift check) runs under the remaining allowance too.
	prepStart := a.now()
	pctx, cancel := a.ctx, func() {}
	if l := st.Limits.MaxActiveSeconds; l > 0 {
		pctx, cancel = context.WithTimeout(a.ctx, time.Duration((l-st.Counters.ActiveSeconds)*float64(time.Second)))
	}
	defer cancel()
	if *refresh {
		if err := pipeline.Refresh(pctx, r, st, *cfg); err != nil {
			if pctx.Err() != nil {
				st.Counters.ActiveSeconds += a.now().Sub(prepStart).Seconds()
				return limitStop()
			}
			return a.errorf("refresh: %v", err)
		}
		fmt.Fprintf(a.stderr, "[resume] new generation %d: repositories re-snapshotted, planning starts again at %s; spend so far is kept\n", st.Generation, st.Cursor.Stage)
	} else if man, err := inputs.Load(filepath.Join(dir, "manifest.json")); err == nil {
		// §9: changed repositories need an explicit new generation; approvals are never inherited.
		if drift, err := inputs.CheckDrift(pctx, man); errors.Is(err, inputs.ErrDrift) {
			return a.errorf("inputs changed since the snapshot (%s): run `shogun resume %s --refresh` for a new generation", strings.Join(drift, ", "), st.RunID)
		}
	}
	if ans != nil {
		if err := a.attachAnswerFiles(dir, ans); err != nil {
			return a.errorf("%v", err)
		}
		if err := pipeline.ApplyAnswers(r, st, ans, a.now()); err != nil {
			return a.errorf("%s: %v", *answers, err)
		}
		fmt.Fprintf(a.stderr, "[resume] %d answer(s) recorded\n", len(ans.Answers))
	} else if st.Status == run.StatusNeedsInput && len(st.Progress.Pending) > 0 && !a.interactive {
		return a.errorf("run %s needs answers: shogun resume %s --answers answers.json (questions in %s)", st.RunID, st.RunID, filepath.Join(dir, "questions.json"))
	}
	st.Counters.ActiveSeconds += a.now().Sub(prepStart).Seconds()
	if pctx.Err() != nil {
		return limitStop()
	}
	if err := r.SaveState(st, a.now()); err != nil {
		return a.errorf("%v", err)
	}
	fmt.Fprintf(a.stderr, "[resume] run %s at %s (generation %d)\n", st.RunID, st.Cursor.Stage, st.Generation)
	return a.runPipeline(r, st, *cfg, !a.interactive, false)
}

func (a *app) cmdStatus(args []string) int {
	fs := a.newFlagSet("status")
	asJSON := fs.Bool("json", false, "print state.json")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitError
	}
	if len(pos) != 1 {
		return a.errorf("usage: shogun status <run-id|dir> [--json]")
	}
	dir, err := run.Resolve(a.cwd, pos[0])
	if err != nil {
		return a.errorf("%v", err)
	}
	r, err := run.Open(dir)
	if err != nil {
		return a.errorf("%v", err)
	}
	st, err := r.LoadState()
	if err != nil {
		return a.errorf("%v", err)
	}
	if *asJSON {
		data, _ := json.MarshalIndent(st, "", " ")
		fmt.Fprintln(a.stdout, string(data))
		return ExitOK
	}
	fmt.Fprintf(a.stdout, "run:        %s\nstatus:     %s\nstage:      %s", st.RunID, st.Status, st.Cursor.Stage)
	if st.Cursor.Step != "" {
		fmt.Fprintf(a.stdout, " / %s", st.Cursor.Step)
	}
	fmt.Fprintf(a.stdout, " (round %d)\ngeneration: %d\ncalls:      %d logical, %d attempts, %.0fs active\ntokens:     %d input, %d output (Claude list-price equivalent $%.2f)\nlimits:     %d logical, %d attempts, %.0fs active (%s)\nupdated:    %s\n",
		st.Cursor.Round, st.Generation, st.Counters.LogicalCalls, st.Counters.Attempts, st.Counters.ActiveSeconds,
		st.Counters.InputTokens, st.Counters.OutputTokens, st.Counters.CostUSD,
		st.Limits.MaxLogicalCalls, st.Limits.MaxAttempts, st.Limits.MaxActiveSeconds, st.Limits.Source, st.UpdatedAt)
	if st.Reason != "" {
		fmt.Fprintf(a.stdout, "reason:     %s\n", st.Reason)
	}
	fmt.Fprintf(a.stdout, "dir:        %s\n", dir)
	return ExitOK
}

func (a *app) cmdList(args []string) int {
	fs := a.newFlagSet("list")
	status := fs.String("status", "", "filter by execution status")
	project := fs.String("project", "", "filter by project")
	dir := fs.String("dir", "", "directory to scan (default: plans_dir from config, else ./docs/plans)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	scan := *dir
	if scan == "" {
		loaded, err := config.Load(a.cwd, config.Overrides{}, a.getenv)
		if err != nil {
			return a.errorf("%v", err)
		}
		scan = loaded.Config.PlansDir
		if scan == "" {
			scan = filepath.Join(a.cwd, "docs", "plans")
		}
	}
	if _, err := os.Stat(scan); err != nil {
		return a.errorf("plans directory %s: %v", scan, err)
	}
	entries, err := library.List(scan)
	if err != nil {
		return a.errorf("%v", err)
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tINTEGRITY\tCREATED\tPROJECT\tTITLE\tPATH")
	n := 0
	for _, e := range entries {
		if *status != "" && e.Status != *status || *project != "" && e.Project != *project {
			continue
		}
		rel, err := filepath.Rel(a.cwd, e.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = e.Path
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(e.Status), e.Integrity, dash(e.Created), dash(e.Project), dash(e.Title), rel)
		n++
	}
	tw.Flush()
	if n == 0 {
		fmt.Fprintf(a.stderr, "no plans in %s\n", scan)
	}
	return ExitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) cmdVerify(args []string) int {
	fs := a.newFlagSet("verify")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitError
	}
	if len(pos) != 1 {
		return a.errorf("usage: shogun verify <plan.md>")
	}
	p := pos[0]
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.cwd, p)
	}
	res, note := library.Verify(p)
	fmt.Fprintf(a.stdout, "%s\t%s\n", res, note)
	return res.ExitCode()
}

func (a *app) cmdConfig(args []string) int {
	fs := a.newFlagSet("config")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	loaded, err := config.Load(a.cwd, config.Overrides{}, a.getenv)
	if err != nil {
		return a.errorf("%v", err)
	}
	for _, f := range loaded.Files {
		fmt.Fprintf(a.stdout, "# read %s\n", f)
	}
	loaded.Describe(a.stdout)
	return ExitOK
}
