package pipeline

import (
	"context"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/run"
	"path/filepath"
	"testing"
)

func TestCostReviewRefreshKeepsFastModeAndBudget(t *testing.T) {
	f := newFast(t, nil, nil)
	st := f.e.State
	f.e.Manifest.Version = inputs.ManifestVersion
	st.Cursor = run.Cursor{Stage: StagePlan}
	st.Limits = run.Limits{MaxLogicalCalls: 6, MaxAttempts: 18, MaxActiveSeconds: 900, Source: "fast"}
	st.Counters.LogicalCalls, st.Counters.Attempts = 2, 2
	if err := f.e.Manifest.Save(filepath.Join(f.e.Run.Dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := Refresh(context.Background(), f.e.Run, st, f.e.Cfg); err != nil {
		t.Fatal(err)
	}
	if st.Cursor.Stage != StagePlan {
		t.Errorf("fast refresh switched to %q without --thorough", st.Cursor.Stage)
	}
	if st.Limits.MaxLogicalCalls != 6 || st.Limits.MaxAttempts != 18 {
		t.Errorf("fast refresh replenished limits: %+v", st.Limits)
	}
}
