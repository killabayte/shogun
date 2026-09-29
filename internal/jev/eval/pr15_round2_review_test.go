package eval

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A valid payload can race cancellation. A last response arriving after the run
// deadline must not turn an expired evaluation into a positive J1 decision.
func TestPR15Round2ReviewLateLastResponseCannotPass(t *testing.T) {
	c := pr15PerfectClient(t)
	transport := c.HTTP.Transport
	calls := 0
	c.HTTP.Transport = pr15Transport(func(r *http.Request) (*http.Response, error) {
		calls++
		res, err := transport.RoundTrip(r)
		if calls == 55 {
			<-r.Context().Done()
		}
		return res, err
	})
	caps := Caps{MaxRequests: 60, MaxElapsed: time.Second}
	r := Run(context.Background(), c, Cases, caps)
	if calls != 55 {
		t.Skipf("host exhausted the allowance before the last-response boundary: calls=%d", calls)
	}
	if ok, why := r.Decision(); ok {
		t.Fatalf("late last response approved after %s (cap %s): %s; stopped_by=%q", r.Elapsed, caps.MaxElapsed, why, r.StoppedBy)
	}
	if r.Skipped == 0 || r.StoppedBy == "" {
		t.Fatalf("deadline loss was not recorded: skipped=%d stopped_by=%q", r.Skipped, r.StoppedBy)
	}
}
