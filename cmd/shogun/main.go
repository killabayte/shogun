// Command shogun plans tasks with a planner model and a reviewer model until the plan is approved.
// P1–P2: configuration, intake, run store, library, doctor with the live config preflight.
// Pipeline stages arrive in P3+.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/provider"
)

// Exit codes (plan §3).
const (
	ExitOK          = 0
	ExitLimit       = 1 // limit or stalemate; draft kept in the run dir
	ExitError       = 2 // configuration / tool / protocol error
	ExitNeedsInput  = 3
	ExitInterrupted = 130
)

// version is set at build time from an exact release tag (make build on v0.1.0 → "0.1.0"). Without
// it the module version Go records is used: the tag for go install …@v0.1.0, a pseudo-version after
// the last tag for any other build in the repository (0.2.1-0.20260928074837-d85b371d6d65).
var version = ""

// revision is set at build time (make build: branch-hash-timestamp); "latest" for plain go build.
var revision = "latest"

// releaseVersion is the version shown by `version` and `--help`.
func releaseVersion() string {
	module := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		module = bi.Main.Version
	}
	return versionFrom(version, module)
}

// versionFrom prefers the build-time version, then the module version; "dev" when neither is known
// (a build outside version control).
func versionFrom(set, module string) string {
	switch {
	case set != "":
		return set
	case module != "" && module != "(devel)":
		return strings.TrimPrefix(module, "v")
	}
	return "dev"
}

// getwd is a hook for tests.
var getwd = os.Getwd

// runnersHook replaces the real, preflight-checked model runners in tests.
var runnersHook func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error)

// stdin and interactive are hooks for tests: questions are asked only on a terminal.
var (
	stdin       io.Reader = os.Stdin
	interactive           = func() bool { fi, err := os.Stdin.Stat(); return err == nil && fi.Mode()&os.ModeCharDevice != 0 }
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	interrupted := ctx.Err() != nil // must be read before stop() cancels the context itself
	stop()
	if interrupted && code != ExitOK {
		code = ExitInterrupted
	}
	os.Exit(code)
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func usage(w io.Writer) {
	fmt.Fprintf(w, `shogun %s — planner ⇄ reviewer plan forge

Usage:
  shogun plan [flags] "<task>"        create a run, snapshot inputs, start planning
  shogun plan [flags] --task-file f   same, task text from a file
  shogun resume <run-id|dir> [flags]  continue a run (answers, refresh, budgets)
  shogun status <run-id|dir> [--json] show run state
  shogun list [--status s] [--project p] [--dir d]   list plans in the library
  shogun stats [--dir d]              time, attempts and tokens of every run in .shogun/runs
  shogun verify <plan.md>             integrity of the approved area vs its receipt
  shogun doctor [--live] [--planner s] [--reviewer s]   check binaries, versions, config; --live certifies a model pair
  shogun config                       print the effective configuration with provenance
  shogun version

Exit codes: 0 ok · 1 limit/stalemate · 2 config/tool/protocol error · 3 needs input · 130 interrupted
`, releaseVersion())
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitError
	}
	cwd, err := getwd()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitError
	}
	app := &app{ctx: ctx, cwd: cwd, stdout: stdout, stderr: stderr, getenv: os.Getenv, now: time.Now,
		stdin: stdin, interactive: interactive(), newRunners: runnersHook}
	switch args[0] {
	case "plan":
		return app.cmdPlan(args[1:])
	case "resume":
		return app.cmdResume(args[1:])
	case "status":
		return app.cmdStatus(args[1:])
	case "list":
		return app.cmdList(args[1:])
	case "stats":
		return app.cmdStats(args[1:])
	case "verify":
		return app.cmdVerify(args[1:])
	case "doctor":
		return app.cmdDoctor(args[1:])
	case "config":
		return app.cmdConfig(args[1:])
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "shogun", releaseVersion(), revision)
		return ExitOK
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return ExitError
	}
}

type app struct {
	ctx         context.Context
	cwd         string
	stdout      io.Writer
	stderr      io.Writer
	getenv      func(string) string
	now         func() time.Time
	stdin       io.Reader
	interactive bool
	newRunners  func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error)
}

func (a *app) errorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "error: "+format+"\n", args...)
	return ExitError
}

// newFlagSet builds a flag set that prints its usage to stderr and never exits the process.
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// parseArgs parses flags that may appear before or after positional arguments
// (`shogun status <id> --json`, `shogun plan "task" --repo x`). Tokens are classified in one pass:
// a `--` terminator makes everything after it positional and is never re-parsed; a non-boolean flag
// consumes the next token as its value (so `--answers --` treats `--` as a value, not a terminator);
// unknown flags produce the standard flag error.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals, flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positionals = append(positionals, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			flagArgs = append(flagArgs, a)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			if err := fs.Parse([]string{a}); err != nil { // standard "flag provided but not defined" / help
				return nil, err
			}
			continue
		}
		flagArgs = append(flagArgs, a)
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positionals, nil
}
