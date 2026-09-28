package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/run"
)

func runCLI(t *testing.T, ws string, args ...string) (int, string, string) {
	t.Helper()
	old := getwd
	getwd = func() (string, error) { return ws, nil }
	defer func() { getwd = old }()
	var out, errb bytes.Buffer
	code := dispatch(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-qm", "i"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func TestParseArgsTerminatorAndValues(t *testing.T) {
	mk := func() (*flag.FlagSet, *string, *bool) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs, fs.String("answers", "", ""), fs.Bool("json", false, "")
	}
	fs, ans, js := mk()
	pos, err := parseArgs(fs, []string{"run-1", "--json", "--", "--answers", "x"})
	if err != nil || !*js || *ans != "" || strings.Join(pos, "|") != "run-1|--answers|x" {
		t.Fatalf("terminator after positional: %v %q json=%v ans=%q", err, pos, *js, *ans)
	}
	fs, ans, js = mk()
	pos, err = parseArgs(fs, []string{"--answers", "--", "run-1", "--json"})
	if err != nil || *ans != "--" || !*js || strings.Join(pos, "|") != "run-1" {
		t.Fatalf("-- as a flag value: %v %q ans=%q json=%v", err, pos, *ans, *js)
	}
	fs, ans, _ = mk()
	pos, err = parseArgs(fs, []string{"--answers=a.json", "task", "words", "--json"})
	if err != nil || *ans != "a.json" || strings.Join(pos, " ") != "task words" {
		t.Fatalf("=value and trailing bool: %v %q ans=%q", err, pos, *ans)
	}
	fs, _, _ = mk()
	if _, err := parseArgs(fs, []string{"run-1", "--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
	fs, _, _ = mk()
	pos, err = parseArgs(fs, []string{"-", "--json"})
	if err != nil || strings.Join(pos, "|") != "-" {
		t.Fatalf("lone dash is positional: %v %q", err, pos)
	}
}

func TestVersionAndUnknownCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	code, out, _ := runCLI(t, t.TempDir(), "version")
	if code != ExitOK || !strings.Contains(out, "shogun") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errs := runCLI(t, t.TempDir(), "frobnicate"); code != ExitError || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown command: %d %q", code, errs)
	}
	if code, _, _ := runCLI(t, t.TempDir()); code != ExitError {
		t.Fatal("no args must be an error")
	}
}

func TestPlanIntakeHappyPathAndStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	repoB := gitRepo(t)
	os.WriteFile(filepath.Join(ws, "spec.md"), []byte("# spec\n"), 0o644)
	os.MkdirAll(filepath.Join(ws, ".shogun"), 0o755)
	os.WriteFile(filepath.Join(ws, ".shogun", "config.toml"), []byte("project = \"demo\"\n"), 0o644)
	useFakeModels(t)
	code, out, errs := runCLI(t, ws, "plan", "Add rate limiting", "--repo", ws, "--repo", repoB, "--input", "spec.md", "--json")
	if code != ExitOK || !strings.Contains(errs, "[intake] complete") || !strings.Contains(errs, "[plan] approved") {
		t.Fatalf("intake: code=%d out=%q err=%q", code, out, errs)
	}
	var res struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Status != "approved" || res.Path != filepath.Join(ws, "docs", "plans", res.RunID+".md") {
		t.Fatalf("json result: %v %+v (%s)", err, res, out)
	}
	runDir := filepath.Join(ws, ".shogun", "runs", res.RunID)
	for _, f := range []string{"task.md", "manifest.json", "config.snapshot.toml", "state.json", filepath.Join("inputs", "01-spec.md"), "PLAN.md", "approval.json"} {
		if _, err := os.Stat(filepath.Join(runDir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if fi, _ := os.Stat(runDir); fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir perm %v", fi.Mode())
	}
	code, out, _ = runCLI(t, ws, "status", res.RunID)
	if code, out, errs := runCLI(t, ws, "verify", res.Path); code != ExitOK || !strings.Contains(out+errs, "valid") {
		t.Fatalf("published plan does not verify: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(strings.TrimSuffix(res.Path, ".md") + ".approval.json"); err != nil {
		t.Fatalf("sidecar receipt: %v", err)
	}
	if code != ExitOK || !strings.Contains(out, "status:     approved") || !strings.Contains(out, "stage:      publish") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, out, _ = runCLI(t, ws, "status", runDir, "--json") // documented order: flags after the id
	if code != ExitOK || !strings.Contains(out, `"run_id": "`+res.RunID+`"`) {
		t.Fatalf("status json: %d %q", code, out)
	}
	man, _ := os.ReadFile(filepath.Join(runDir, "manifest.json"))
	if !strings.Contains(string(man), `"repo-2"`) || !strings.Contains(string(man), `"is_git": true`) {
		t.Fatalf("manifest should contain two git repos: %s", man)
	}
}

// Without a certified config preflight no model is called: the run fails with a hint.
func TestPlanRequiresPreflight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	os.MkdirAll(filepath.Join(ws, ".shogun"), 0o755)
	// A binary that exists but has no preflight record for this configuration.
	os.WriteFile(filepath.Join(ws, ".shogun", "config.toml"), []byte("claude_command = \"/bin/echo\"\ncodex_command = \"/bin/echo\"\n"), 0o644)
	code, _, errs := runCLI(t, ws, "plan", "Task")
	if code != ExitError || !strings.Contains(errs, "doctor --live") {
		t.Fatalf("plan without preflight: %d %q", code, errs)
	}
}

func TestPlanNeedsInputAndConfigErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	code, _, errs := runCLI(t, ws, "plan", "--input", "missing.md", "Task")
	if code != ExitNeedsInput || !strings.Contains(errs, "needs_input") {
		t.Fatalf("missing input must yield exit 3: %d %q", code, errs)
	}
	if code, _, errs := runCLI(t, ws, "plan", "--reviewer", "codex/gpt-6-sol:low", "Task"); code != ExitError || !strings.Contains(errs, "below the hardcoded minimum") {
		t.Fatalf("effort floor must fail before any run is created: %d %q", code, errs)
	}
	if code, _, errs := runCLI(t, ws, "plan", "--planner", "codex/x:high", "Task"); code != ExitError || !strings.Contains(errs, "roles are fixed") {
		t.Fatalf("role swap must fail: %d %q", code, errs)
	}
	if code, _, errs := runCLI(t, ws, "plan"); code != ExitError || !strings.Contains(errs, "task is required") {
		t.Fatalf("missing task: %d %q", code, errs)
	}
	entries, _ := os.ReadDir(filepath.Join(ws, ".shogun", "runs"))
	if len(entries) != 1 { // only the needs_input run was created; config errors create nothing
		t.Fatalf("expected exactly one run dir, got %d", len(entries))
	}
}

func TestConfigCommandShowsProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // GitHub's ubuntu runners set it; the test uses ~/.config
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".config", "shogun"), 0o755)
	os.WriteFile(filepath.Join(home, ".config", "shogun", "config.toml"), []byte("plans_dir = \"~/plans\"\n"), 0o644)
	code, out, _ := runCLI(t, ws, "config")
	if code != ExitOK || !strings.Contains(out, "plans_dir") || !strings.Contains(out, "# file:") || !strings.Contains(out, "# default") {
		t.Fatalf("config: %d %q", code, out)
	}
	os.WriteFile(filepath.Join(home, ".config", "shogun", "config.toml"), []byte("bogus = 1\n"), 0o644)
	if code, _, errs := runCLI(t, ws, "config"); code != ExitError || !strings.Contains(errs, "unknown key") {
		t.Fatalf("unknown key: %d %q", code, errs)
	}
}

func TestListAndVerify(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	plans := filepath.Join(ws, "docs", "plans")
	os.MkdirAll(plans, 0o755)
	plan := "---\ntitle: Demo\nplan_id: p1\nrevision: 1\ncreated: 2026-09-23\nstatus: planned\nproject: demo\n---\n<!-- shogun:plan:begin -->\nbody\n<!-- shogun:plan:end -->\n"
	p := filepath.Join(plans, "p1.md")
	os.WriteFile(p, []byte(plan), 0o644)
	code, out, _ := runCLI(t, ws, "verify", "docs/plans/p1.md")
	if code != 2 || !strings.HasPrefix(out, "unverifiable") {
		t.Fatalf("verify without receipt: %d %q", code, out)
	}
	doc, _ := library.Parse([]byte(plan))
	rc, _ := json.Marshal(library.Receipt{SchemaVersion: 1, PlanID: "p1", Revision: 1, BodySHA256: doc.BodySHA256, ImmutableMetadata: library.ImmutableMetadata(doc.Frontmatter)})
	os.WriteFile(library.ReceiptPath(p), rc, 0o644)
	if code, out, _ := runCLI(t, ws, "verify", p); code != 0 || !strings.HasPrefix(out, "valid") {
		t.Fatalf("verify valid: %d %q", code, out)
	}
	code, out, _ = runCLI(t, ws, "list")
	if code != ExitOK || !strings.Contains(out, "planned") || !strings.Contains(out, "valid") || !strings.Contains(out, "Demo") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, ws, "list", "--status", "done"); code != ExitOK || strings.Contains(out, "Demo") {
		t.Fatalf("list filter: %d %q", code, out)
	}
}

// The version comes from the release tag when make sets it, else from the module version Go records
// (a tag for go install @vX, a pseudo-version after the last tag otherwise), never a stale constant.
func TestVersionFrom(t *testing.T) {
	for _, c := range []struct{ set, module, want string }{
		{"0.2.1", "v0.2.1", "0.2.1"},
		{"", "v0.2.1", "0.2.1"},
		{"", "v0.2.1-0.20260928074837-d85b371d6d65", "0.2.1-0.20260928074837-d85b371d6d65"},
		{"", "v0.2.1-0.20260928074837-d85b371d6d65+dirty", "0.2.1-0.20260928074837-d85b371d6d65+dirty"},
		{"", "(devel)", "dev"},
		{"", "", "dev"},
	} {
		if got := versionFrom(c.set, c.module); got != c.want {
			t.Errorf("versionFrom(%q, %q) = %q, want %q", c.set, c.module, got, c.want)
		}
	}
}

// A closed question is asked again in the terminal until an option is chosen (at most three tries).
func TestTerminalAskerReasksClosedQuestion(t *testing.T) {
	var out strings.Builder
	a := &terminalAsker{in: bufio.NewReader(strings.NewReader("reference\n2\n")), out: &out}
	got, err := a.Ask(context.Background(), []run.Pending{{ID: "Q-001", Stage: "plan", Origin: "shogun", Question: "What is in-1 for?",
		Options: []string{"reference: x", "authoritative: y"}, Blocking: true, Closed: true}})
	if err != nil || got["Q-001"] != "authoritative: y" || !strings.Contains(out.String(), "answer with one of the numbers 1-2") {
		t.Fatalf("%v %q\n%s", err, got, out.String())
	}
}
