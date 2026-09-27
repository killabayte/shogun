package inputs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMaterializeFilesAndURLs(t *testing.T) {
	run := t.TempDir()
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "spec.md"), []byte("# spec\n"), 0o644)
	os.WriteFile(filepath.Join(cwd, "bin.dat"), []byte("ab\x00cd"), 0o644)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello"))
		case "/forbidden":
			w.WriteHeader(403)
		case "/login":
			w.Write([]byte(`<html><form><input type="password"><button>Log in</button></form></html>`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	m := NewMaterializer()
	srcs, err := m.Materialize(context.Background(), run, cwd, []string{"spec.md", srv.URL + "/ok", "missing.md", "bin.dat", srv.URL + "/forbidden", srv.URL + "/login", srv.URL + "/nope"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	want := []string{"ok", "ok", "error", "error", "error", "error", "error"}
	for i, s := range srcs {
		if s.Status != want[i] {
			t.Errorf("input %d (%s): status %s want %s (%s)", i, s.Origin, s.Status, want[i], s.Error)
		}
	}
	if srcs[0].SHA256 == "" || srcs[0].StoredPath != filepath.Join("inputs", "01-spec.md") {
		t.Errorf("file not stored: %+v", srcs[0])
	}
	if _, err := os.Stat(filepath.Join(run, srcs[1].StoredPath)); err != nil || srcs[1].ContentType != "text/plain" {
		t.Errorf("url not stored: %+v", srcs[1])
	}
	if fi, _ := os.Stat(filepath.Join(run, srcs[0].StoredPath)); fi.Mode().Perm() != 0o600 {
		t.Errorf("stored input perm %v", fi.Mode())
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestRepoManifestDetectsDirtyContentAndUntracked(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "sample.txt"), []byte("one\n"), 0o644)
	gitInit(t, root)
	ctx := context.Background()
	clean, err := RepoManifest(ctx, "repo-1", root)
	if err != nil || !clean.IsGit || clean.Head == "" {
		t.Fatalf("clean manifest: %+v %v", clean, err)
	}
	// F-13: two different edits of the same dirty file must yield different fingerprints,
	// even though `git status` shows the same "M sample.txt" for both.
	os.WriteFile(filepath.Join(root, "sample.txt"), []byte("two\n"), 0o644)
	dirtyA, _ := RepoManifest(ctx, "repo-1", root)
	os.WriteFile(filepath.Join(root, "sample.txt"), []byte("three\n"), 0o644)
	dirtyB, _ := RepoManifest(ctx, "repo-1", root)
	if dirtyA.Fingerprint == clean.Fingerprint || dirtyA.Fingerprint == dirtyB.Fingerprint {
		t.Fatalf("dirty content changes not detected: clean=%s a=%s b=%s", clean.Fingerprint, dirtyA.Fingerprint, dirtyB.Fingerprint)
	}
	// untracked file content change
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("x"), 0o644)
	untrA, _ := RepoManifest(ctx, "repo-1", root)
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("y"), 0o644)
	untrB, _ := RepoManifest(ctx, "repo-1", root)
	if untrA.Fingerprint == dirtyB.Fingerprint || untrA.Fingerprint == untrB.Fingerprint {
		t.Fatal("untracked content changes not detected")
	}
	// .shogun artifacts are excluded
	os.MkdirAll(filepath.Join(root, ".shogun", "runs"), 0o755)
	os.WriteFile(filepath.Join(root, ".shogun", "runs", "x.json"), []byte("{}"), 0o644)
	excl, _ := RepoManifest(ctx, "repo-1", root)
	if excl.Fingerprint != untrB.Fingerprint {
		t.Fatal(".shogun/ must not affect the fingerprint")
	}
	// in a git repo, .gitignore is the authority: untracked vendor/ content counts
	os.MkdirAll(filepath.Join(root, "vendor", "lib"), 0o755)
	os.WriteFile(filepath.Join(root, "vendor", "lib", "x.go"), []byte("v1"), 0o644)
	vend1, _ := RepoManifest(ctx, "repo-1", root)
	os.WriteFile(filepath.Join(root, "vendor", "lib", "x.go"), []byte("v2"), 0o644)
	vend2, _ := RepoManifest(ctx, "repo-1", root)
	if vend1.Fingerprint == excl.Fingerprint || vend1.Fingerprint == vend2.Fingerprint {
		t.Fatal("non-ignored untracked files under vendor/ must affect the git fingerprint")
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("vendor/\n"), 0o644)
	ign1, _ := RepoManifest(ctx, "repo-1", root)
	os.WriteFile(filepath.Join(root, "vendor", "lib", "x.go"), []byte("v3"), 0o644)
	ign2, _ := RepoManifest(ctx, "repo-1", root)
	if ign1.Fingerprint != ign2.Fingerprint {
		t.Fatal("ignored files must not affect the git fingerprint")
	}
	excl = ign2
	m := &Manifest{Version: ManifestVersion, Repos: []Repo{excl}}
	m.ComputeFingerprint()
	if changed, err := CheckDrift(ctx, m); err != nil || changed != nil {
		t.Fatalf("no drift expected: %v %v", changed, err)
	}
	os.WriteFile(filepath.Join(root, "sample.txt"), []byte("four\n"), 0o644)
	if _, err := CheckDrift(ctx, m); !errors.Is(err, ErrDrift) {
		t.Fatalf("drift expected, got %v", err)
	}
}

func TestRepoManifestUnreadableUntrackedIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitInit(t, root)
	secret := filepath.Join(root, "secret.txt")
	os.WriteFile(secret, []byte("s"), 0o000)
	t.Cleanup(func() { os.Chmod(secret, 0o644) })
	if _, err := RepoManifest(context.Background(), "repo-1", root); err == nil {
		t.Fatal("an unreadable untracked file must fail the fingerprint, not silently skip")
	}
}

func TestCheckTextContract(t *testing.T) {
	if err := checkText([]byte("привет, world\n")); err != nil {
		t.Fatalf("valid UTF-8 rejected: %v", err)
	}
	for name, data := range map[string][]byte{"invalid utf8": {'a', 0xff, 'b'}, "nul": {'a', 0, 'b'}, "pdf": []byte("%PDF-1.7 ..."), "png": {0x89, 'P', 'N', 'G', 1}} {
		if err := checkText(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRepoManifestUnbornHeadAndNonGit(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "-C", root, "init", "-q").Run()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	r, err := RepoManifest(context.Background(), "repo-1", root)
	if err != nil || r.Head != "" || r.UntrackedSHA256 == "" {
		t.Fatalf("unborn head handling: %+v %v", r, err)
	}
	plain := t.TempDir()
	os.WriteFile(filepath.Join(plain, "f.txt"), []byte("f"), 0o644)
	os.MkdirAll(filepath.Join(plain, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(plain, "node_modules", "x", "big.js"), []byte("junk"), 0o644)
	p1, err := RepoManifest(context.Background(), "repo-2", plain)
	if err != nil || p1.IsGit || p1.InventorySHA256 == "" {
		t.Fatalf("non-git manifest: %+v %v", p1, err)
	}
	os.WriteFile(filepath.Join(plain, "node_modules", "x", "big.js"), []byte("junk2"), 0o644)
	p2, _ := RepoManifest(context.Background(), "repo-2", plain)
	if p1.Fingerprint != p2.Fingerprint {
		t.Fatal("node_modules must be excluded from the inventory")
	}
	if _, err := RepoManifest(context.Background(), "repo-3", filepath.Join(plain, "missing")); err == nil {
		t.Fatal("missing root must error")
	}
}

// Live 2026-09-27 (PORTALS-3426): a clean repository was rendered "with local changes" because the
// digests of an empty diff and of no untracked files are set too.
func TestDirtyIgnoresEmptyDigests(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "sample.txt"), []byte("one\n"), 0o644)
	gitInit(t, root)
	r, err := RepoManifest(context.Background(), "repo-1", root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dirty() {
		t.Fatalf("clean repository reported dirty: %+v", r)
	}
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("x"), 0o644)
	if r, _ = RepoManifest(context.Background(), "repo-1", root); !r.Dirty() {
		t.Fatal("untracked file not reported")
	}
}
