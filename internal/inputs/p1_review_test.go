package inputs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestP1ReviewNonIgnoredUntrackedBuildFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "README.md"), []byte("repo"), 0600)
	gitInit(t, root)
	os.MkdirAll(filepath.Join(root, "build"), 0700)
	p := filepath.Join(root, "build", "release.sh")
	os.WriteFile(p, []byte("echo A"), 0600)
	a, err := RepoManifest(context.Background(), "repo-1", root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("echo B"), 0600)
	b, err := RepoManifest(context.Background(), "repo-1", root)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("changed non-ignored untracked build/release.sh left fingerprint unchanged")
	}
}
func TestP1ReviewInvalidUTF8Rejected(t *testing.T) {
	if err := checkText([]byte{'a', 0xff, 'b'}); err == nil {
		t.Fatal("invalid UTF-8 accepted as text")
	}
}
