package inputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is the manifest entry for one repository root.
type Repo struct {
	ID              string `json:"id"`
	Root            string `json:"root"`
	IsGit           bool   `json:"is_git"`
	Head            string `json:"head,omitempty"`
	DiffSHA256      string `json:"diff_sha256,omitempty"`      // git diff HEAD --binary (tracked changes, staged+unstaged)
	UntrackedSHA256 string `json:"untracked_sha256,omitempty"` // contents of non-ignored untracked files
	InventorySHA256 string `json:"inventory_sha256,omitempty"` // non-git: (path,size,mtime) listing
	Fingerprint     string `json:"fingerprint"`
	Note            string `json:"note,omitempty"`
}

// emptySHA256 is the digest of no bytes: an empty diff or no untracked files.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Dirty reports whether a git repository had tracked changes or untracked files. The digests are
// always set, so it compares them with the digest of nothing rather than checking for presence.
func (r Repo) Dirty() bool {
	return (r.DiffSHA256 != "" && r.DiffSHA256 != emptySHA256) || (r.UntrackedSHA256 != "" && r.UntrackedSHA256 != emptySHA256)
}

// EmptyTree is git's well-known empty tree object (used for unborn HEAD).
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// excludedDirs applies to non-git inventories only: without ignore rules these trees are noise.
// In git repositories .gitignore is the authority; Shogun excludes only its own artifacts there.
var excludedDirs = map[string]bool{".git": true, ".shogun": true, "node_modules": true, "vendor": true, "dist": true, "build": true}

// shogunOwnDirs are excluded from git untracked scans (run dirs, config) regardless of ignore rules.
var shogunOwnDirs = map[string]bool{".git": true, ".shogun": true}

// RepoManifest fingerprints a repository root. Content of dirty tracked files and untracked
// files is hashed; unchanged tracked files are represented by HEAD (no full rescan). Files listed in
// exclude (absolute paths: the plan Shogun will publish and its receipt, §3) are left out, so
// Shogun's own output is never input drift.
func RepoManifest(ctx context.Context, id, root string, exclude ...string) (Repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Repo{}, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return Repo{}, fmt.Errorf("repo %s: not a directory: %v", root, err)
	}
	r := Repo{ID: id, Root: abs}
	skip := map[string]bool{}
	for _, p := range exclude {
		skip[normalize(p)] = true
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		r.IsGit = true
		if err := gitFingerprint(ctx, &r, skip); err != nil {
			return r, err
		}
	} else {
		sum, err := inventory(abs, skip)
		if err != nil {
			return r, err
		}
		r.InventorySHA256 = sum
		r.Note = "non-git inventory: (path,size,mtime) only; a same-size same-mtime edit is not detected"
	}
	r.Fingerprint = r.ComputeFingerprint()
	return r, nil
}

// ComputeFingerprint derives the repository fingerprint from the recorded head, change and
// inventory digests, so a stored Repo can be checked for internal consistency without its tree.
func (r Repo) ComputeFingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%v\x00%s\x00%s\x00%s\x00%s", r.IsGit, r.Head, r.DiffSHA256, r.UntrackedSHA256, r.InventorySHA256)
	return hex.EncodeToString(h.Sum(nil))
}

func git(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "--no-optional-locks"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_EXTERNAL_DIFF=", "GIT_PAGER=cat", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// normalize resolves symlinks of p, or of its directory when p does not exist yet.
func normalize(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

func gitFingerprint(ctx context.Context, r *Repo, skip map[string]bool) error {
	head, err := git(ctx, r.Root, "rev-parse", "--verify", "-q", "HEAD")
	base := EmptyTree
	if err == nil {
		base = strings.TrimSpace(string(head))
		r.Head = base
	} else {
		r.Head = "" // unborn
	}
	diff, err := git(ctx, r.Root, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--full-index", base)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(diff)
	r.DiffSHA256 = hex.EncodeToString(sum[:])
	untracked, err := git(ctx, r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	h := sha256.New()
	for _, rel := range strings.Split(string(untracked), "\x00") {
		if rel == "" || underShogunOwn(rel) || skip[filepath.Join(r.Root, rel)] {
			continue
		}
		p := filepath.Join(r.Root, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("untracked %s: %w", rel, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("untracked %s: %w", rel, err)
			}
			fmt.Fprintf(h, "%s\x00symlink:%s\n", rel, target)
		case fi.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("untracked %s: %w", rel, err)
			}
			fh := sha256.New()
			_, cerr := io.Copy(fh, f)
			f.Close()
			if cerr != nil {
				return fmt.Errorf("untracked %s: %w", rel, cerr)
			}
			fmt.Fprintf(h, "%s\x00%s\n", rel, hex.EncodeToString(fh.Sum(nil)))
		default:
			fmt.Fprintf(h, "%s\x00special:%s\n", rel, fi.Mode().Type())
		}
	}
	r.UntrackedSHA256 = hex.EncodeToString(h.Sum(nil))
	return nil
}

func underShogunOwn(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if shogunOwnDirs[part] {
			return true
		}
	}
	return false
}

// inventory hashes (relpath, size, mtime_ns) of a non-git tree with documented exclusions.
func inventory(root string, skip map[string]bool) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && excludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skip[p] {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			fmt.Fprintf(h, "%s\x00symlink:%s\n", rel, target)
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrDrift is returned by CheckDrift when any repo or input changed since the manifest.
var ErrDrift = errors.New("inputs changed since the manifest was taken")

// CheckDrift recomputes repo fingerprints and compares to the stored manifest.
// Explicit inputs are snapshots and do not drift; repos do.
func CheckDrift(ctx context.Context, m *Manifest) ([]string, error) {
	var changed []string
	for _, r := range m.Repos {
		cur, err := RepoManifest(ctx, r.ID, r.Root, m.Exclude...)
		if err != nil {
			return nil, err
		}
		if cur.Fingerprint != r.Fingerprint {
			changed = append(changed, r.ID+" ("+r.Root+")")
		}
	}
	if len(changed) > 0 {
		return changed, fmt.Errorf("%w: %s", ErrDrift, strings.Join(changed, ", "))
	}
	return nil, nil
}
