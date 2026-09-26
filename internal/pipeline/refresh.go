package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// generationArtifacts are the per-generation results; a refresh moves them to gen-<n>/.
var generationArtifacts = []string{"research", "outline", "detail", "steps", "reviews", "integration", "web", "web.json",
	"candidate.md", "approval.json", "questions.json"}

// Refresh starts a new generation after input drift (§9): new repository snapshots and a fresh
// research; old approvals are not inherited. Spend counters are kept, and the new generation's
// reserve sits on top of them. Explicit inputs are snapshots and stay; user decisions stay.
func Refresh(ctx context.Context, r *run.Run, st *run.State, cfg config.Config) error {
	manPath := filepath.Join(r.Dir, "manifest.json")
	man, err := inputs.Load(manPath)
	if err != nil {
		return err
	}
	for i, repo := range man.Repos {
		cur, err := inputs.RepoManifest(ctx, repo.ID, repo.Root, man.Exclude...)
		if err != nil {
			return err
		}
		man.Repos[i] = cur
	}
	man.ComputeFingerprint()
	archive := filepath.Join(r.Dir, fmt.Sprintf("gen-%d", st.Generation))
	if err := os.MkdirAll(archive, 0o700); err != nil {
		return err
	}
	for _, name := range generationArtifacts {
		err := os.Rename(filepath.Join(r.Dir, name), filepath.Join(archive, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := man.Save(manPath); err != nil {
		return err
	}
	c, l := st.Counters, &st.Limits
	l.BaseLogicalCalls, l.BaseAttempts = c.LogicalCalls, c.Attempts
	l.MaxLogicalCalls = c.LogicalCalls + 4*cfg.ReviewRounds
	l.ExplicitAttempts = l.ExplicitAttempts || l.Source == "flag"
	l.Source = "pre-outline"
	if !l.ExplicitAttempts {
		l.MaxAttempts = c.Attempts + provider.MaxAttempts*4*cfg.ReviewRounds
	}
	st.Generation++
	st.Progress = run.Progress{NextFinding: st.Progress.NextFinding, NextQuestion: st.Progress.NextQuestion}
	st.Publish.Done = false
	st.Hashes = map[string]string{"manifest": man.Fingerprint}
	st.Cursor = run.Cursor{Stage: StageResearch}
	st.Status, st.Reason = run.StatusRunning, ""
	return nil
}
