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
	"strings"
	"syscall"
	"time"
)

// Exit codes (plan §3).
const (
	ExitOK          = 0
	ExitLimit       = 1 // limit or stalemate; draft kept in the run dir
	ExitError       = 2 // configuration / tool / protocol error
	ExitNeedsInput  = 3
	ExitInterrupted = 130
)

const version = "0.1.0-dev"

// getwd is a hook for tests.
var getwd = os.Getwd

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
  shogun verify <plan.md>             integrity of the approved area vs its receipt
  shogun doctor [--live]              check binaries, versions, flags, config, directories
  shogun config                       print the effective configuration with provenance
  shogun version

Exit codes: 0 ok · 1 limit/stalemate · 2 config/tool/protocol error · 3 needs input · 130 interrupted
`, version)
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
	app := &app{ctx: ctx, cwd: cwd, stdout: stdout, stderr: stderr, getenv: os.Getenv, now: time.Now}
	switch args[0] {
	case "plan":
		return app.cmdPlan(args[1:])
	case "resume":
		return app.cmdResume(args[1:])
	case "status":
		return app.cmdStatus(args[1:])
	case "list":
		return app.cmdList(args[1:])
	case "verify":
		return app.cmdVerify(args[1:])
	case "doctor":
		return app.cmdDoctor(args[1:])
	case "config":
		return app.cmdConfig(args[1:])
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "shogun", version)
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
	ctx    context.Context
	cwd    string
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	now    func() time.Time
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
