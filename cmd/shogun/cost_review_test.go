package main

import (
	"encoding/json"
	"github.com/killabayte/shogun/internal/run"
	"path/filepath"
	"testing"
)

func TestCostReviewDefaultFastBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "Add a version flag")
	if code != ExitOK {
		t.Fatalf("plan failed: %d %s %s", code, out, errs)
	}
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(filepath.Join(ws, ".shogun", "runs", res.RunID))
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Limits.MaxAttempts <= 0 || st.Limits.MaxAttempts > 4 {
		t.Errorf("physical attempt cap = %d, expected at most four including retries", st.Limits.MaxAttempts)
	}
	if st.Limits.MaxActiveSeconds <= 0 || st.Limits.MaxActiveSeconds > 600 {
		t.Errorf("active-time cap = %.0fs, exceeds ten-minute requirement", st.Limits.MaxActiveSeconds)
	}
}
