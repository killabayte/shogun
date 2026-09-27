package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type costRound2Transport func(*http.Request) (*http.Response, error)

func (f costRound2Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCostRound2IntakeHonorsActiveDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	called, deadline, canceled := false, false, false
	http.DefaultTransport = costRound2Transport(func(r *http.Request) (*http.Response, error) {
		called = true
		_, deadline = r.Context().Deadline()
		select {
		case <-r.Context().Done():
			canceled = true
			return nil, r.Context().Err()
		case <-time.After(150 * time.Millisecond):
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("Version must be printed.\n")), Request: r}, nil
		}
	})
	code, _, errs := runCLI(t, ws, "plan", "--auto", "--max-time", "40ms", "--input", "https://example.test/spec", "Add version")
	if called && (!deadline || !canceled) {
		t.Errorf("intake ran past its allowance: deadline=%v canceled=%v", deadline, canceled)
	}
	if code != ExitLimit {
		t.Errorf("deadline must pause as limit, got %d: %s", code, errs)
	}
}
