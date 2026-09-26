// Package run owns the run directory: ids, layout, the single-writer lock,
// atomic checkpoints of state.json and artifact writes with private permissions.
package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// StateVersion is bumped on incompatible changes to State.
const StateVersion = 1

// Status values of a run.
type Status string

const (
	StatusIntake     Status = "intake"
	StatusRunning    Status = "running"
	StatusNeedsInput Status = "needs_input"
	StatusPaused     Status = "paused"
	StatusApproved   Status = "approved"
	StatusFailed     Status = "failed"
)

// Cursor is where the pipeline stands.
type Cursor struct {
	Stage string `json:"stage"`
	Step  string `json:"step,omitempty"`
	Round int    `json:"round"`
}

// Counters are cumulative spend; they survive resume and refresh.
type Counters struct {
	LogicalCalls  int     `json:"logical_calls"`
	Attempts      int     `json:"attempts"`
	ReviewRounds  int     `json:"review_rounds"`
	ActiveSeconds float64 `json:"active_seconds"`
}

// Limits are the effective budgets (0 = not yet derived).
type Limits struct {
	MaxLogicalCalls  int     `json:"max_logical_calls"`
	MaxAttempts      int     `json:"max_attempts"`
	MaxActiveSeconds float64 `json:"max_active_seconds"`
	CallDeadlineSecs float64 `json:"call_deadline_seconds"`
	Source           string  `json:"source"` // "pre-outline" | "derived" | "flag"
}

// State is the checkpointed run state (state.json).
type State struct {
	Version    int               `json:"version"`
	RunID      string            `json:"run_id"`
	Generation int               `json:"generation"`
	Status     Status            `json:"status"`
	Reason     string            `json:"reason,omitempty"`
	Cursor     Cursor            `json:"cursor"`
	Counters   Counters          `json:"counters"`
	Limits     Limits            `json:"limits"`
	Hashes     map[string]string `json:"hashes,omitempty"` // manifest, requirements revision, candidate…
	Progress   Progress          `json:"progress"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}

// Progress is the planning pipeline's checkpointed position inside the stages (P3+).
type Progress struct {
	Revisions      map[string]int    `json:"revisions"`       // stage -> latest revision written
	Approved       map[string]int    `json:"approved"`        // stage -> approved revision
	Rounds         map[string]int    `json:"rounds"`          // stage -> review rounds spent on the unit
	Stalemate      map[string]string `json:"stalemate"`       // stage -> signature of the previous review round
	Ledger         []Finding         `json:"ledger"`          // every finding ever raised, with its status
	NextFinding    int               `json:"next_finding"`    // last assigned F-NNN
	GateNotes      []string          `json:"gate_notes"`      // mechanical problems carried into the next round
	Pending        []Pending         `json:"pending"`         // questions waiting for the user
	NextQuestion   int               `json:"next_question"`   // last assigned Q-NNN
	QuestionRounds int               `json:"question_rounds"` // question rounds asked so far (max 2)
}

// Finding is one ledger entry. Only the reviewer closes a finding (resolved|rejected); a finding
// missing from a later review stays open.
type Finding struct {
	ID              string   `json:"id"`
	Stage           string   `json:"stage"`
	Severity        string   `json:"severity"`
	TargetID        string   `json:"target_id"`
	Problem         string   `json:"problem"`
	RequestedChange string   `json:"requested_change"`
	Evidence        []string `json:"evidence"`
	Status          string   `json:"status"` // open | resolved | rejected
	OpenedIn        string   `json:"opened_in"`
	ClosedIn        string   `json:"closed_in,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	Disputes        int      `json:"disputes,omitempty"` // planner disputes that left the finding open
}

// Pending is a question Shogun has not got an answer for yet. ID is assigned by Shogun.
type Pending struct {
	ID                 string   `json:"id"`
	Stage              string   `json:"stage"`
	Origin             string   `json:"origin"` // planner | reviewer | shogun
	Question           string   `json:"question"`
	Why                string   `json:"why"`
	Impact             string   `json:"impact"`
	Options            []string `json:"options"`
	ProposedAssumption string   `json:"proposed_assumption"`
	Blocking           bool     `json:"blocking"`
}

// Run is an opened run directory. Hold the lock for the whole lifetime of a writer.
type Run struct {
	Dir  string
	lock *os.File
}

// Subdirectories created for every run.
var subdirs = []string{"inputs", "requirements", "research", "outline", "steps", "calls", "reviews"}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify makes a short, filesystem-safe, lowercase ASCII slug (max 32 chars).
func Slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '/' || r == '.':
			b.WriteByte('-')
		default:
			// non-ASCII letters are dropped: ids stay ASCII regardless of task language
		}
	}
	out := strings.Trim(slugRe.ReplaceAllString(b.String(), "-"), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	if out == "" {
		out = "plan"
	}
	return out
}

// NewID returns YYYYMMDD-HHMMSS-<slug>-<4 hex>. rnd may be nil for crypto/rand.
func NewID(now time.Time, task string, rnd io.Reader) string {
	if rnd == nil {
		rnd = rand.Reader
	}
	var b [2]byte
	_, _ = io.ReadFull(rnd, b[:])
	return fmt.Sprintf("%s-%s-%s", now.Format("20060102-150405"), Slugify(task), hex.EncodeToString(b[:]))
}

// RunsRoot is <workspace>/.shogun/runs.
func RunsRoot(workspace string) string { return filepath.Join(workspace, ".shogun", "runs") }

// Create makes the run directory tree with private permissions. It fails if the dir exists.
func Create(runsRoot, id string) (*Run, error) {
	dir := filepath.Join(runsRoot, id)
	if err := os.MkdirAll(runsRoot, 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	for _, d := range subdirs {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Run{Dir: dir}, nil
}

// Resolve turns a run id or a directory path into an existing run directory.
func Resolve(workspace, idOrDir string) (string, error) {
	cands := []string{idOrDir, filepath.Join(RunsRoot(workspace), idOrDir)}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "state.json")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				return "", err
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("run %q not found (tried %s)", idOrDir, strings.Join(cands, ", "))
}

// Open attaches to an existing run directory (does not lock).
func Open(dir string) (*Run, error) {
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		return nil, fmt.Errorf("not a run dir: %w", err)
	}
	return &Run{Dir: dir}, nil
}

// Lock acquires the exclusive single-writer lock (non-blocking). ErrLocked when another writer holds it.
func (r *Run) Lock() error {
	if r.lock != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := flock(f); err != nil {
		f.Close()
		return err
	}
	r.lock = f
	return nil
}

// ErrLocked means another process holds the run lock.
var ErrLocked = errors.New("run is locked by another process")

// Unlock releases the lock (idempotent).
func (r *Run) Unlock() {
	if r.lock != nil {
		_ = funlock(r.lock)
		r.lock.Close()
		r.lock = nil
	}
}

// NewState returns an initial state for a fresh run.
func NewState(id string, now time.Time) *State {
	ts := now.UTC().Format(time.RFC3339)
	return &State{Version: StateVersion, RunID: id, Generation: 1, Status: StatusIntake,
		Cursor: Cursor{Stage: "intake"}, Hashes: map[string]string{}, CreatedAt: ts, UpdatedAt: ts}
}

// SaveState checkpoints state.json atomically (temp file + fsync + rename).
func (r *Run) SaveState(st *State, now time.Time) error {
	st.Version = StateVersion
	st.UpdatedAt = now.UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(r.Dir, "state.json"), data, 0o600)
}

// LoadState reads and validates state.json.
func (r *Run) LoadState() (*State, error) {
	data, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	if st.Version != StateVersion {
		return nil, fmt.Errorf("state.json version %d is not supported (want %d)", st.Version, StateVersion)
	}
	if st.Hashes == nil {
		st.Hashes = map[string]string{}
	}
	return &st, nil
}

// WriteArtifact writes a file under the run dir (0600), creating parent dirs (0700), atomically.
func (r *Run) WriteArtifact(rel string, data []byte) error {
	p := filepath.Join(r.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(p, data, 0o600)
}

// WriteFileAtomic writes data to a temp file in the same directory, fsyncs, then renames over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
