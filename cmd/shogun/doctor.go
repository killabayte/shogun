package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

type check struct {
	name   string
	ok     bool
	detail string
}

// cmdDoctor runs the offline checks (binaries, versions, feature names, config, directories) and
// reports the config preflight. --live runs one control call per model and writes the preflight.
func (a *app) cmdDoctor(args []string) int {
	fs := a.newFlagSet("doctor")
	live := fs.Bool("live", false, "also run one short control call per model and record the config preflight")
	planner := fs.String("planner", "", "certify this planner instead of the configured one (claude/<model>:<effort>)")
	reviewer := fs.String("reviewer", "", "certify this reviewer instead of the configured one (codex/<model>:<effort>)")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	var checks []check
	add := func(name string, ok bool, detail string) { checks = append(checks, check{name, ok, detail}) }

	loaded, err := config.Load(a.cwd, config.Overrides{Planner: *planner, Reviewer: *reviewer}, a.getenv)
	if err != nil {
		add("config", false, err.Error())
	} else {
		add("config", true, fmt.Sprintf("planner=%s reviewer=%s (%d file(s))", loaded.Config.Planner, loaded.Config.Reviewer, len(loaded.Files)))
	}
	cfg := config.Default()
	if loaded != nil {
		cfg = loaded.Config
	}
	provider.StripFromChildren(cfg.StripEnv...) // before any model call, doctor --live included
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()

	claudePath, claudeVer, err := resolveBinary(ctx, cfg.ClaudeCommand)
	add("claude binary", err == nil, describeBin(claudePath, claudeVer, err, "install Claude Code or set claude_command"))
	codexPath, codexVer, err := resolveBinary(ctx, cfg.CodexCommand)
	hint := "set codex_command to the executable (e.g. /Applications/ChatGPT.app/Contents/Resources/codex); shell functions are not visible to shogun"
	add("codex binary", err == nil, describeBin(codexPath, codexVer, err, hint))
	features := ""
	if err == nil {
		var missing []string
		var ferr error
		features, missing, ferr = checkCodexFeatures(ctx, codexPath)
		switch {
		case ferr != nil:
			add("codex features", false, ferr.Error())
		case len(missing) > 0:
			add("codex features", false, "unknown feature names for this version: "+strings.Join(missing, ", ")+" (isolation profile cannot be applied)")
		default:
			add("codex features", true, strings.Join(provider.CodexDisabledFeatures, ","))
		}
	}
	if cfg.PlansDir != "" {
		add("plans_dir", dirWritable(cfg.PlansDir), cfg.PlansDir)
	} else {
		add("plans_dir", true, "library disabled; plans go to ./docs/plans")
	}
	add("runs dir", dirWritable(filepath.Join(a.cwd, ".shogun", "runs")) || !exists(filepath.Join(a.cwd, ".shogun")), filepath.Join(a.cwd, ".shogun", "runs"))
	for _, v := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		if a.getenv(v) != "" {
			add("env "+v, true, "set in the parent environment; shogun strips it from child processes (subscription auth is used)")
		}
	}
	allOK := true
	for _, c := range checks {
		mark := "ok  "
		if !c.ok {
			mark, allOK = "FAIL", false
		}
		fmt.Fprintf(a.stdout, "%s %-16s %s\n", mark, c.name, c.detail)
	}
	items := preflightItems(cfg, claudePath, claudeVer, codexPath, codexVer, features, a.getenv)
	fp := provider.Fingerprint(items)
	recPath := preflightPath(a.cwd, fp)
	if !*live {
		rec, err := provider.LoadPreflight(recPath)
		status := "certified for this configuration"
		if err == nil {
			err = rec.Verify(fp)
		}
		if err != nil {
			status = err.Error()
		}
		fmt.Fprintf(a.stdout, "info %-16s %s\n", "preflight", status)
		if !allOK {
			return ExitError
		}
		return ExitOK
	}
	if !allOK {
		fmt.Fprintln(a.stdout, "skip live             fix the failed checks first")
		return ExitError
	}
	return a.livePreflight(cfg, claudePath, codexPath, fp, items, recPath)
}

// preflightItems is everything the config preflight depends on (§8): a change of any of them
// invalidates the record.
func preflightItems(cfg config.Config, claudePath, claudeVer, codexPath, codexVer, features string, getenv func(string) string) map[string]string {
	return map[string]string{
		"claude.path": claudePath, "claude.version": claudeVer,
		"codex.path": codexPath, "codex.version": codexVer, "codex.features": features,
		"planner": cfg.Planner.String(), "reviewer": cfg.Reviewer.String(),
		"argv":           provider.ArgvTemplate(plannerRequest(cfg), reviewerRequest(cfg)),
		"env.CODEX_HOME": getenv("CODEX_HOME"), "env.CLAUDE_CONFIG_DIR": getenv("CLAUDE_CONFIG_DIR"),
	}
}

func plannerRequest(cfg config.Config) provider.Request {
	return provider.Request{Model: cfg.Planner.Model, Effort: string(cfg.Planner.Effort)}
}

func reviewerRequest(cfg config.Config) provider.Request {
	return provider.Request{Model: cfg.Reviewer.Model, Effort: string(cfg.Reviewer.Effort)}
}

func (a *app) livePreflight(cfg config.Config, claudePath, codexPath, fp string, items map[string]string, recPath string) int {
	dir, err := os.MkdirTemp("", "shogun-preflight-")
	if err != nil {
		fmt.Fprintln(a.stderr, "shogun:", err)
		return ExitError
	}
	fmt.Fprintf(a.stdout, "live preflight: one control call per model (traces in %s)\n", dir)
	fmt.Fprintf(a.stdout, "limits: 1 physical attempt per model, %s in total\n", provider.PreflightDeadline)
	checks, spend := provider.RunPreflightSpend(a.ctx, &provider.Claude{Bin: claudePath}, &provider.Codex{Bin: codexPath},
		plannerRequest(cfg), reviewerRequest(cfg), dir)
	c := run.Counters{LogicalCalls: 2, Attempts: spend.Attempts, ActiveSeconds: spend.Seconds}
	pipeline.AddUsages(&c, spend.Usages)
	defer fmt.Fprintf(a.stdout, "spend: %s\n", pipeline.SpendLine(c, run.Limits{MaxLogicalCalls: 2, MaxAttempts: 2, MaxActiveSeconds: provider.PreflightDeadline.Seconds()}))
	rec := &provider.Preflight{Version: provider.PreflightVersion, Fingerprint: fp, Items: items, CreatedAt: time.Now().UTC().Format(time.RFC3339), Checks: checks}
	for _, c := range checks {
		fmt.Fprintf(a.stdout, "%-10s %-6s/%-12s %s\n", c.Verdict, c.Provider, c.Name, c.Detail)
	}
	if err := os.MkdirAll(filepath.Dir(recPath), 0o700); err != nil {
		fmt.Fprintln(a.stderr, "shogun:", err)
		return ExitError
	}
	b, _ := json.MarshalIndent(rec, "", " ")
	if err := os.WriteFile(recPath, b, 0o600); err != nil {
		fmt.Fprintln(a.stderr, "shogun:", err)
		return ExitError
	}
	if err := rec.Verify(fp); err != nil {
		fmt.Fprintln(a.stdout, "preflight: NOT certified —", err)
		return ExitError
	}
	fmt.Fprintln(a.stdout, "preflight: certified, recorded in", recPath)
	return ExitOK
}

func describeBin(path, ver string, err error, hint string) string {
	if err != nil {
		return err.Error() + "; " + hint
	}
	return fmt.Sprintf("%s (%s)", path, ver)
}

// resolveBinary finds the executable (absolute path or PATH lookup) and reads --version.
func resolveBinary(ctx context.Context, cmd string) (string, string, error) {
	path := cmd
	if !strings.ContainsRune(cmd, os.PathSeparator) {
		p, err := exec.LookPath(cmd)
		if err != nil {
			return "", "", fmt.Errorf("%q not found in PATH", cmd)
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if _, err := os.Stat(path); err != nil {
		return "", "", err
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return path, "", fmt.Errorf("%s --version failed: %v", path, err)
	}
	return path, strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

func checkCodexFeatures(ctx context.Context, codexPath string) (string, []string, error) {
	cmd := exec.CommandContext(ctx, codexPath, "features", "list")
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("codex features list: %v", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 {
			have[f[0]] = true
		}
	}
	var missing []string
	for _, f := range provider.CodexDisabledFeatures {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	return out.String(), missing, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func dirWritable(p string) bool {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(p, ".shogun-doctor-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
