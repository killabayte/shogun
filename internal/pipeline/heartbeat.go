package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HeartbeatEvery is how often a running model call reports that it is alive (a variable for tests).
var HeartbeatEvery = 30 * time.Second

// heartbeat prints one line per interval while a model call runs: the role, how long the call has
// taken, the call's attempt, what is left of the run's active time and when the CLI last wrote output. It
// reads only the attempt files the provider already writes and never estimates progress or cost.
// The returned stop waits for the goroutine, so no line is printed after the call returns.
func (e *Engine) heartbeat(role, dir string, maxAttempts int) (stop func()) {
	if e.Log == nil || HeartbeatEvery <= 0 {
		return func() {}
	}
	start := time.Now()
	limit, spent := e.State.Limits.MaxActiveSeconds, e.active()
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(HeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				e.logf("  … %s", heartbeatLine(role, dir, start, maxAttempts, limit, spent))
			}
		}
	}()
	return func() { close(done); <-finished }
}

func heartbeatLine(role, dir string, start time.Time, maxAttempts int, limit, spent float64) string {
	now := time.Now()
	attempt, last := latestAttempt(dir)
	line := fmt.Sprintf("%s working %s", role, round(now.Sub(start)))
	if attempt > 0 {
		line += fmt.Sprintf(", attempt %d/%d of this call", attempt, maxAttempts)
	}
	if limit > 0 {
		left := limit - spent - now.Sub(start).Seconds()
		line += fmt.Sprintf("; %.1f of %s min active left", max(left, 0)/60, minutesText(limit))
	}
	if last.IsZero() {
		line += "; no CLI output yet"
	} else {
		line += fmt.Sprintf("; last CLI output %s ago", round(now.Sub(last)))
	}
	return line
}

// latestAttempt returns the newest attempt number under a call directory and the last time the CLI
// wrote to that attempt's output files.
func latestAttempt(dir string) (int, time.Time) {
	n := 0
	for exists(filepath.Join(dir, fmt.Sprintf("attempt-%d", n+1))) {
		n++
	}
	var last time.Time
	if n > 0 {
		for _, f := range []string{"stdout.jsonl", "stderr.log"} {
			if fi, err := os.Stat(filepath.Join(dir, fmt.Sprintf("attempt-%d", n), f)); err == nil && fi.Size() > 0 && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
	}
	return n, last
}

func round(d time.Duration) time.Duration { return d.Round(time.Second) }
