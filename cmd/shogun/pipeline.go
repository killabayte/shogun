package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// runners returns the planner and reviewer for cfg after checking that the config preflight
// certifies exactly this configuration (§8). Tests replace it.
func (a *app) runners(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, map[string]string, error) {
	if a.newRunners != nil {
		p, r, err := a.newRunners(ctx, cfg)
		return p, r, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	claudePath, claudeVer, err := resolveBinary(ctx, cfg.ClaudeCommand)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("claude: %v", err)
	}
	codexPath, codexVer, err := resolveBinary(ctx, cfg.CodexCommand)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("codex: %v (set codex_command)", err)
	}
	features, _, err := checkCodexFeatures(ctx, codexPath)
	if err != nil {
		return nil, nil, nil, err
	}
	fp := provider.Fingerprint(preflightItems(cfg, claudePath, claudeVer, codexPath, codexVer, features, a.getenv))
	rec, err := provider.LoadPreflight(preflightPath(a.cwd, fp))
	if err == nil && rec == nil { // a record written before per-configuration files
		rec, err = provider.LoadPreflight(filepath.Join(a.cwd, ".shogun", "preflight.json"))
	}
	if err == nil {
		err = rec.Verify(fp)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	provider.StripFromChildren(cfg.JevKeyEnv) // the models must not be able to call Jev
	return &provider.Claude{Bin: claudePath}, &provider.Codex{Bin: codexPath}, map[string]string{"claude": claudeVer, "codex": codexVer}, nil
}

// runPipeline executes the stages for an opened, locked run and maps the outcome to an exit code.
func (a *app) runPipeline(r *run.Run, st *run.State, cfg config.Config, auto, asJSON bool) int {
	man, err := inputs.Load(filepath.Join(r.Dir, "manifest.json"))
	if err != nil {
		return a.errorf("%v", err)
	}
	task := string(mustReadFile(filepath.Join(r.Dir, "task.md")))
	if missing := pipeline.UnavailableInputs(man); len(missing) > 0 {
		st.Status, st.Reason = run.StatusNeedsInput, pipeline.MissingInputsReason(missing)
		_ = r.SaveState(st, a.now())
		fmt.Fprintf(a.stderr, "run %s → needs_input: %s\n", st.RunID, st.Reason)
		return ExitNeedsInput
	}
	planner, reviewer, versions, err := a.runners(a.ctx, cfg)
	if err != nil {
		st.Status, st.Reason = run.StatusFailed, "preflight: "+err.Error()
		_ = r.SaveState(st, a.now())
		if asJSON {
			fmt.Fprintf(a.stdout, `{"run_id":%q,"status":%q,"path":"","reason":%q}`+"\n", st.RunID, st.Status, st.Reason)
		}
		return a.errorf("%v", err)
	}
	e := &pipeline.Engine{Run: r, State: st, Manifest: man, Task: task, Cfg: cfg, Planner: planner, Reviewer: reviewer,
		Log: a.stderr, Now: a.now, Project: projectName(cfg, a.cwd), CLIVersions: versions}
	if !auto {
		e.Asker = &terminalAsker{in: bufio.NewReader(a.stdin), out: a.stderr}
	}
	o := e.Execute(a.ctx)
	code := ExitError
	path := ""
	switch {
	case o.Status == run.StatusApproved:
		code, path = ExitOK, st.Publish.Path
	case o.Status == run.StatusNeedsInput:
		code = ExitNeedsInput
		// The answers file goes into the run directory: a file added to a studied repository would
		// itself be input drift.
		fmt.Fprintf(a.stderr, "answer the questions in %s, save the answers as %s and run: shogun resume %s --answers %s\n",
			filepath.Join(r.Dir, "questions.json"), filepath.Join(r.Dir, "answers.json"), st.RunID, filepath.Join(r.Dir, "answers.json"))
	case o.Status == run.StatusPaused && strings.HasPrefix(o.Reason, "not_implemented"):
		code = ExitError
	case o.Status == run.StatusPaused && strings.HasPrefix(o.Reason, "canceled"):
		code = ExitInterrupted
	case o.Status == run.StatusPaused:
		code = ExitLimit
	}
	fmt.Fprintf(a.stderr, "spend: %s\n", pipeline.SpendLine(st.Counters, st.Limits))
	if asJSON {
		fmt.Fprintf(a.stdout, `{"run_id":%q,"status":%q,"path":%q,"reason":%q}`+"\n", st.RunID, o.Status, path, o.Reason)
	} else {
		fmt.Fprintf(a.stderr, "run %s → %s: %s (%s)\n", st.RunID, o.Status, o.Reason, r.Dir)
		if path != "" {
			fmt.Fprintln(a.stdout, path)
		}
	}
	return code
}

// preflightPath keeps one certificate per configuration fingerprint, so certifying another model
// pair (doctor --live --planner … --reviewer …) does not replace the configured pair's record.
func preflightPath(workspace, fingerprint string) string {
	return filepath.Join(workspace, ".shogun", "preflight", fingerprint[:16]+".json")
}

// projectName is the configured project, or a safe slug of the workspace directory (§3).
func projectName(cfg config.Config, workspace string) string {
	if cfg.Project != "" {
		return cfg.Project
	}
	return run.Slugify(filepath.Base(workspace))
}

// outputPath is the plan's single primary file (§3): --out, else plans_dir/<project>/<run-id>.md,
// else <workspace>/docs/plans/<run-id>.md.
func outputPath(out string, cfg config.Config, workspace, runID string) string {
	switch {
	case out != "" && filepath.IsAbs(out):
		return out
	case out != "":
		return filepath.Join(workspace, out)
	case cfg.PlansDir != "":
		return filepath.Join(cfg.PlansDir, projectName(cfg, workspace), runID+".md")
	}
	return filepath.Join(workspace, "docs", "plans", runID+".md")
}

// terminalAsker shows a batch of questions with the recommendation and reads one line per answer;
// an empty line accepts the proposed assumption.
type terminalAsker struct {
	in  *bufio.Reader
	out io.Writer
}

func (t *terminalAsker) Ask(ctx context.Context, qs []run.Pending) (map[string]string, error) {
	answers := map[string]string{}
	fmt.Fprintf(t.out, "\n%d question(s) from the %s stage:\n", len(qs), qs[0].Stage)
	for _, q := range qs {
		fmt.Fprintf(t.out, "\n%s (%s%s): %s\n  why: %s\n  impact: %s\n", q.ID, q.Origin, map[bool]string{true: ", blocking"}[q.Blocking], q.Question, q.Why, q.Impact)
		for i, o := range q.Options {
			fmt.Fprintf(t.out, "  %d) %s\n", i+1, o)
		}
		if q.ProposedAssumption != "" {
			fmt.Fprintf(t.out, "  recommended: %s (press Enter to accept)\n", q.ProposedAssumption)
		}
		var line string
		for try := 1; ; try++ {
			fmt.Fprint(t.out, "> ")
			l, err := t.in.ReadString('\n')
			if err != nil && l == "" {
				return nil, fmt.Errorf("reading the answer to %s: %v", q.ID, err)
			}
			line = strings.TrimSpace(l)
			if n := optionNumber(line, len(q.Options)); n > 0 {
				line = q.Options[n-1]
			}
			if line == "" { // Enter explicitly accepts the recommendation; without one it stays unanswered
				line = q.ProposedAssumption
			}
			// A closed question takes only one of its options: ask again here, before any model call.
			if !q.Closed || try == 3 || closedOption(q.Options, line) {
				break
			}
			fmt.Fprintf(t.out, "  answer with one of the numbers 1-%d or an option's exact text\n", len(q.Options))
		}
		answers[q.ID] = line
	}
	return answers, nil
}

func closedOption(options []string, s string) bool {
	for _, o := range options {
		if strings.EqualFold(strings.Join(strings.Fields(o), " "), strings.Join(strings.Fields(s), " ")) {
			return true
		}
	}
	return false
}

func optionNumber(s string, n int) int {
	var k int
	if _, err := fmt.Sscanf(s, "%d", &k); err == nil && fmt.Sprint(k) == s && k >= 1 && k <= n {
		return k
	}
	return 0
}

func mustReadFile(p string) []byte { b, _ := os.ReadFile(p); return b }

// attachAnswerFiles stores files named in answers.json as new snapshot inputs ans-N (§7) so both
// models read them like any explicit input. An unavailable file is an error, not a silent skip.
func (a *app) attachAnswerFiles(runDir string, ans *schema.Answers) error {
	var files []string
	for _, x := range ans.Answers {
		files = append(files, x.Files...)
	}
	if len(files) == 0 {
		return nil
	}
	manPath := filepath.Join(runDir, "manifest.json")
	man, err := inputs.Load(manPath)
	if err != nil {
		return err
	}
	base := 0
	for _, s := range man.Inputs {
		if strings.HasPrefix(s.ID, "ans-") {
			base++
		}
	}
	sub := fmt.Sprintf("answers-%d", base+1)
	srcs, err := inputs.NewMaterializer().Materialize(a.ctx, filepath.Join(runDir, sub), a.cwd, files)
	if err != nil {
		return fmt.Errorf("answer files: %w", err)
	}
	for i := range srcs {
		srcs[i].ID = fmt.Sprintf("ans-%d", base+i+1)
		srcs[i].StoredPath = filepath.Join(sub, srcs[i].StoredPath)
		fmt.Fprintf(a.stderr, "[resume] %s %s → %s\n", srcs[i].ID, srcs[i].Origin, srcs[i].StoredPath)
	}
	man.Inputs = append(man.Inputs, srcs...)
	man.ComputeFingerprint()
	return man.Save(manPath)
}
