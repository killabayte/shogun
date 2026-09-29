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
	c := jev.New(key) // official endpoint, pinned model, no retries
	// The workspace's jev_deny applies on top of the built-in guard, as it would in a run.
	if cwd, err := os.Getwd(); err == nil {
		if loaded, err := config.Load(cwd, config.Overrides{}, os.Getenv); err == nil {
			c.Deny, _ = jev.CompileDeny(loaded.Config.JevDeny)
		}
	}
	var dump strings.Builder
	if refused, err := Outgoing(&dump, Cases, c.Deny); err != nil || refused > 0 {
		t.Fatalf("the outbound guard refuses %d state(s) (%v); nothing was sent — read the dump: JEV_EVAL_DUMP", refused, err)
	}
	rep := Run(context.Background(), c, Cases, DefaultCaps)
	rep.WriteTable(os.Stdout)
	out := os.Getenv("JEV_EVAL_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "shogun-jev-eval-"+time.Now().UTC().Format("20060102T150405")+".json")
	}
	if err := os.WriteFile(out, rep.JSON(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", out)
	if rep.Errors > 0 || rep.Skipped > 0 {
		t.Errorf("%d error(s), %d skipped: the run is incomplete", rep.Errors, rep.Skipped)
	}
}
