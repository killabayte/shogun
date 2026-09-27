package run

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlugifyAndID(t *testing.T) {
	cases := map[string]string{
		"Добавить rate limiting в api-gateway": "rate-limiting-api-gateway",
		"  Hello,   World! ":                   "hello-world",
		"":                                     "plan",
		"a_very/long.task name that goes on and on forever": "a-very-long-task-name-that-goes",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	id := NewID(time.Date(2026, 9, 23, 14, 30, 0, 0, time.UTC), "Rate limit", bytes.NewReader([]byte{0xa7, 0xc9}))
	if id != "20260923-143000-rate-limit-a7c9" {
		t.Fatalf("id = %s", id)
	}
}

func TestCreateLayoutPermissionsAndAtomicState(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".shogun", "runs")
	r, err := Create(root, "20260923-000000-x-0000")
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(r.Dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("run dir perm %v err %v", st.Mode(), err)
	}
	for _, d := range subdirs {
		if _, err := os.Stat(filepath.Join(r.Dir, d)); err != nil {
			t.Errorf("missing %s", d)
		}
	}
	state := NewState("20260923-000000-x-0000", time.Now())
	if err := r.SaveState(state, time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(r.Dir, "state.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state perm %v", fi.Mode())
	}
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	back, err := r.LoadState()
	if err != nil || back.RunID != state.RunID || back.Status != StatusIntake {
		t.Fatalf("roundtrip failed: %+v %v", back, err)
	}
	if _, err := Create(root, "20260923-000000-x-0000"); err == nil {
		t.Fatal("creating an existing run must fail")
	}
	if dir, err := Resolve(filepath.Dir(filepath.Dir(root)), "20260923-000000-x-0000"); err != nil || dir != r.Dir {
		t.Fatalf("Resolve by id: %s %v", dir, err)
	}
	if err := r.WriteArtifact("calls/abc/prompt.md", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(r.Dir, "calls", "abc")); fi.Mode().Perm() != 0o700 {
		t.Errorf("artifact dir perm %v", fi.Mode())
	}
}

func TestLoadStateRejectsWrongVersion(t *testing.T) {
	r, _ := Create(t.TempDir(), "20260923-000000-v-0000")
	if err := os.WriteFile(filepath.Join(r.Dir, "state.json"), []byte(`{"version": 99, "run_id": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadState(); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	r, _ := Create(t.TempDir(), "20260923-000000-l-0000")
	if err := r.Lock(); err != nil {
		t.Fatal(err)
	}
	second := &Run{Dir: r.Dir}
	if err := second.Lock(); err != ErrLocked {
		t.Fatalf("second lock should fail with ErrLocked, got %v", err)
	}
	r.Unlock()
	if err := second.Lock(); err != nil {
		t.Fatalf("lock after unlock should succeed: %v", err)
	}
	second.Unlock()
}

func TestWriteFileAtomicReplacesWholeContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.json")
	if err := WriteFileAtomic(p, []byte("long long content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "short" {
		t.Fatalf("got %q", got)
	}
}
