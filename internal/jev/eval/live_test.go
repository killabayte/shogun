//go:build jevlive

package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/jev"
)

// The live J0 evaluation (design §7): opt-in by build tag, the key from JEV_DEFAULT_TEST or
// TYPESAFE_API_KEY, the §7 caps, the report printed and written as JSON to $JEV_EVAL_OUT (default:
// a file in the temp directory). Run it deliberately:
//
//	go test -tags jevlive -run TestLiveEvaluation -v ./internal/jev/eval
func TestLiveEvaluation(t *testing.T) {
	key := os.Getenv("JEV_DEFAULT_TEST")
	if key == "" {
		key = os.Getenv(jev.DefaultKeyEnv)
	}
	if key == "" {
		t.Skip("no key in JEV_DEFAULT_TEST or " + jev.DefaultKeyEnv)
	}
	// Permission to send is established first, and it fails closed: the workspace is explicit
	// (JEV_EVAL_WORKSPACE, else the repository root above this package), its configuration must
	// load, and its jev_deny applies on top of the built-in guard exactly as in a run.
	ws := os.Getenv("JEV_EVAL_WORKSPACE")
	if ws == "" {
		ws = repoRoot(t)
	}
	loaded, err := config.Load(ws, config.Overrides{}, os.Getenv)
	if err != nil {
		t.Fatalf("configuration of %s did not load; nothing sent: %v", ws, err)
	}
	deny, err := jev.CompileDeny(loaded.Config.JevDeny)
	if err != nil {
		t.Fatalf("jev_deny did not compile; nothing sent: %v", err)
	}
	t.Logf("workspace %s; config files %v; jev_deny patterns %d", ws, loaded.Files, len(deny))
	c := jev.New(key) // official endpoint, pinned model, no retries
	c.Deny = deny

	// The audit — every byte that would be sent, with the guard's verdict — is written before
	// anything is sent, refusals included; if it cannot be written, nothing is sent.
	out := os.Getenv("JEV_EVAL_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "shogun-jev-eval-"+time.Now().UTC().Format("20060102T150405")+".json")
	}
	audit := os.Getenv("JEV_EVAL_DUMP")
	if audit == "" {
		audit = strings.TrimSuffix(out, ".json") + ".outgoing.txt"
	}
	var dump strings.Builder
	refused, derr := Outgoing(&dump, Cases, c.Deny)
	if werr := os.WriteFile(audit, []byte(dump.String()), 0o600); werr != nil {
		t.Fatalf("the outgoing audit could not be written to %s; nothing sent: %v", audit, werr)
	}
	t.Logf("outgoing audit written to %s", audit)
	if derr != nil || refused > 0 {
		t.Fatalf("the outbound guard refuses %d request(s) (%v); nothing sent — read %s", refused, derr, audit)
	}

	rep := Run(context.Background(), c, Cases, DefaultCaps)
	rep.WriteTable(os.Stdout)
	if err := os.WriteFile(out, rep.JSON(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", out)
	if rep.Errors > 0 || rep.Skipped > 0 {
		t.Errorf("%d error(s), %d skipped: the run is incomplete", rep.Errors, rep.Skipped)
	}
}

// repoRoot walks up from the package directory to the nearest directory holding go.mod or .shogun.
func repoRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("no working directory: %v", err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		for _, marker := range []string{"go.mod", ".shogun"} {
			if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no go.mod or .shogun above %s; set JEV_EVAL_WORKSPACE", dir)
		}
	}
}
