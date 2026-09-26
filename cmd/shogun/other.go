package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/library"
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
	if *answers != "" {
		data, err := os.ReadFile(*answers)
		if err != nil {
			return a.errorf("%v", err)
		}
		if _, err := schema.Parse[schema.Answers](schema.KindAnswers, data); err != nil {
			return a.errorf("%s: %v", *answers, err)
		}
		fmt.Fprintf(a.stderr, "[resume] answers file is valid (%s)\n", *answers)
	}
	_ = refresh
	_ = maxCalls
	_ = maxTime
	fmt.Fprintf(a.stderr, "[resume] run %s is %s at %s (generation %d); pipeline resume arrives in P3\n", st.RunID, st.Status, st.Cursor.Stage, st.Generation)
	return ExitError
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
	fmt.Fprintf(a.stdout, " (round %d)\ngeneration: %d\ncalls:      %d logical, %d attempts, %.0fs active\nlimits:     %d logical, %d attempts (%s)\nupdated:    %s\n",
		st.Cursor.Round, st.Generation, st.Counters.LogicalCalls, st.Counters.Attempts, st.Counters.ActiveSeconds,
		st.Limits.MaxLogicalCalls, st.Limits.MaxAttempts, st.Limits.Source, st.UpdatedAt)
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
