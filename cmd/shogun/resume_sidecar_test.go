package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/run"
)

// A run whose manifest predates S0 excludes only the plan and the receipt from repository
// fingerprints. When its publication into the studied repository is interrupted after the
// manifest sidecar was installed, `shogun resume` must finish the triplet without model calls
// instead of reading its own sidecar as input drift and demanding a new generation (§9).
func TestResumeRecoversLegacySidecarGap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	outpath := filepath.Join(ws, "docs", "plans", "legacy.md")
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "--out", outpath, "Add a version flag")
	if code != ExitOK {
		t.Fatalf("setup plan: code=%d out=%q err=%q", code, out, errs)
	}
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.RunID == "" {
		t.Fatalf("run id: %v %q", err, out)
	}
	dir := filepath.Join(ws, ".shogun", "runs", res.RunID)
	manPath := filepath.Join(dir, "manifest.json")
	man, err := inputs.Load(manPath)
	if err != nil {
		t.Fatal(err)
	}
	man.Exclude = []string{outpath, library.ReceiptPath(outpath)} // the pre-S0 shape
	if err := man.Save(manPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatal(err)
	}
	// The crash: the sidecar is installed, the receipt and the plan are not.
	for _, p := range []string{outpath, library.ReceiptPath(outpath), library.ManifestPath(outpath)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(library.ManifestPath(outpath), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.Status = run.StatusRunning
	st.Publish.Done = false
	st.Cursor = run.Cursor{Stage: pipeline.StagePublish}
	if err := r.SaveState(st, time.Now()); err != nil {
		t.Fatal(err)
	}

	code, out, errs = runCLI(t, ws, "resume", res.RunID)
	if code != ExitOK || strings.Contains(errs, "--refresh") {
		t.Fatalf("legacy publication was not recovered: code=%d out=%q err=%q", code, out, errs)
	}
	if code, out, errs := runCLI(t, ws, "verify", "--require-manifest", outpath); code != ExitOK || !strings.HasPrefix(out, "valid") {
		t.Fatalf("recovered triplet does not verify: %d %q %q", code, out, errs)
	}
	if st, err = r.LoadState(); err != nil || st.Status != run.StatusApproved || !st.Publish.Done {
		t.Fatalf("run state after recovery: %v status=%s done=%v", err, st.Status, st.Publish.Done)
	}
	got, err := os.ReadFile(library.ManifestPath(outpath))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("sidecar bytes changed during recovery: %v", err)
	}
	if fi, err := os.Stat(library.ManifestPath(outpath)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode after recovery: %v %v", err, fi.Mode())
	}
}
