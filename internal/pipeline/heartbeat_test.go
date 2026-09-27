package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// slowRunner writes an attempt's output like a CLI does and takes a while before answering.
type slowRunner struct {
	inner *script
	wait  time.Duration
}

func (s slowRunner) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	dir := filepath.Join(req.Dir, "attempt-1")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "stdout.jsonl"), []byte("{}\n"), 0o600)
	time.Sleep(s.wait)
	return s.inner.Run(ctx, req)
}

// A long call reports that it is alive: role, elapsed time, attempt, active time left and the last
// CLI output. Nothing is printed once the call has returned.
func TestHeartbeatWhileACallRuns(t *testing.T) {
	old := HeartbeatEvery
	HeartbeatEvery = 20 * time.Millisecond
	t.Cleanup(func() { HeartbeatEvery = old })
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	f.e.State.Limits.MaxAttempts, f.e.State.Limits.MaxActiveSeconds = 4, 600
	f.e.Planner = slowRunner{f.planner, 150 * time.Millisecond}
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	log := f.log.String()
	if !strings.Contains(log, "… planner working") || !strings.Contains(log, "attempt 1/3 of this call") ||
		!strings.Contains(log, "of 10 min active left") || !strings.Contains(log, "last CLI output") {
		t.Fatalf("no heartbeat:\n%s", log)
	}
	if strings.Contains(log, "… reviewer working") {
		t.Fatal("a fast call printed a heartbeat")
	}
	after := strings.Index(log, "reviewer call")
	if strings.Contains(log[after:], "… planner") {
		t.Fatalf("heartbeat outlived its call:\n%s", log)
	}
}
