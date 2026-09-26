package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func passingRecord(fp string) *Preflight {
	p := &Preflight{Version: PreflightVersion, Fingerprint: fp}
	for _, n := range requiredChecks {
		prov, name, _ := strings.Cut(n, "/")
		p.Checks = append(p.Checks, Check{Provider: prov, Name: name, Verdict: "pass"})
	}
	return p
}

func TestPreflightVerify(t *testing.T) {
	fp := Fingerprint(map[string]string{"codex.version": "0.155.0-alpha.16"})
	if err := passingRecord(fp).Verify(fp); err != nil {
		t.Fatal(err)
	}
	stale := Fingerprint(map[string]string{"codex.version": "0.156.0"})
	failed := passingRecord(fp)
	failed.Checks[4].Verdict = "unverified"
	missing := passingRecord(fp)
	missing.Checks = missing.Checks[:len(missing.Checks)-1]
	empty := &Preflight{Version: PreflightVersion, Fingerprint: fp}
	for name, tc := range map[string]struct {
		p  *Preflight
		fp string
	}{"no record": {nil, fp}, "stale": {passingRecord(fp), stale}, "unverified check": {failed, fp},
		"missing check": {missing, fp}, "empty diagnostics": {empty, fp}} {
		if err := tc.p.Verify(tc.fp); !errors.Is(err, ErrPreflight) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// stubRunner answers with the fixture's codewords (read from the prompt paths) and writes a trace.
type stubRunner struct {
	wrongWords bool
	stdout     string
}

var (
	rePath   = regexp.MustCompile(`/\S+?\.(txt|md)`)
	reTarget = regexp.MustCompile(`/\S+PROBE_WRITE\.txt`)
)

func (s stubRunner) Run(ctx context.Context, req Request) (*Result, error) {
	words := map[string]string{}
	for i, p := range rePath.FindAllString(req.Prompt, 3) {
		b, _ := os.ReadFile(p)
		w := strings.TrimSpace(strings.TrimPrefix(string(b), "codeword: "))
		if s.wrongWords {
			w = "GUESS"
		}
		words[[]string{"repo_a", "repo_b", "input"}[i]] = w
	}
	dir := filepath.Join(req.Dir, "attempt-1")
	os.MkdirAll(dir, 0o700)
	// {TARGET} in the canned trace stands for the fixture's probe file.
	target := reTarget.FindString(req.Prompt)
	os.WriteFile(filepath.Join(dir, "stdout.jsonl"), []byte(strings.ReplaceAll(s.stdout, "{TARGET}", target)), 0o600)
	os.WriteFile(filepath.Join(dir, "stderr.log"), nil, 0o600)
	payload, _ := json.Marshal(map[string]any{"codewords": words})
	return &Result{Payload: payload, Dir: dir}, nil
}

func verdicts(checks []Check) map[string]string {
	out := map[string]string{}
	for _, c := range checks {
		out[c.Provider+"/"+c.Name] = c.Verdict
	}
	return out
}

func TestPreflightNeedsRecordedEvidence(t *testing.T) {
	denial := `{"type":"item.completed","item":{"type":"command_execution","command":"/bin/zsh -lc 'printf probe > {TARGET}'","aggregated_output":"zsh:1: operation not permitted: {TARGET}\n","exit_code":1,"status":"completed"}}`
	got := verdicts(RunPreflight(context.Background(), stubRunner{}, stubRunner{stdout: denial}, Request{}, Request{}, t.TempDir()))
	for _, n := range requiredChecks {
		if got[n] != "pass" {
			t.Errorf("%s = %q, want pass (%v)", n, got[n], got)
		}
	}
	// The model never attempted the write: nothing exercised the sandbox → unverified.
	got = verdicts(RunPreflight(context.Background(), stubRunner{}, stubRunner{}, Request{}, Request{}, t.TempDir()))
	if got["codex/write-denied"] != "unverified" {
		t.Errorf("write-denied without a recorded denial = %q", got["codex/write-denied"])
	}
	// Codewords that do not match the random fixture prove no read.
	got = verdicts(RunPreflight(context.Background(), stubRunner{wrongWords: true}, stubRunner{wrongWords: true, stdout: denial}, Request{}, Request{}, t.TempDir()))
	if got["claude/reads"] != "unverified" || got["codex/reads"] != "unverified" {
		t.Errorf("reads with guessed codewords: %v", got)
	}
}

// A runner that writes into the fixture fails no-write even if its call "succeeds".
type writingRunner struct{ stubRunner }

func (w writingRunner) Run(ctx context.Context, req Request) (*Result, error) {
	os.WriteFile(filepath.Join(req.Roots[0], "PROBE_WRITE.txt"), []byte("probe"), 0o600)
	return w.stubRunner.Run(ctx, req)
}

func TestPreflightDetectsWrites(t *testing.T) {
	got := verdicts(RunPreflight(context.Background(), writingRunner{}, stubRunner{}, Request{}, Request{}, t.TempDir()))
	if got["claude/no-write"] != "fail" {
		t.Errorf("no-write = %q", got["claude/no-write"])
	}
}

// Real traces: the P2 live preflight (apply_patch router error) and the P0b smoke (shell redirect).
func TestWriteDenialOnRecordedTraces(t *testing.T) {
	for _, tc := range []struct{ dir, target, want string }{
		{"testdata/p2/preflight-codex/", "<pf>/codex/fx/repo-a/PROBE_WRITE.txt", "apply_patch"},
		{"testdata/p0b/codex-final-full-agentsoff/", "/work/codex-final-full-agentsoff/fx/repo-a/PROBE_WRITE.txt", "shell write"},
	} {
		stdout, _ := os.ReadFile(tc.dir + "stdout.jsonl")
		stderr, _ := os.ReadFile(tc.dir + "stderr.log")
		if got := writeDenial(stdout, stderr, tc.target); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %s evidence", tc.dir, got, tc.want)
		}
		if got := writeDenial(stdout, stderr, "/elsewhere/PROBE_WRITE.txt"); got != "" {
			t.Errorf("%s: evidence for an unrelated path: %q", tc.dir, got)
		}
	}
}

func TestRedirectsTo(t *testing.T) {
	const tg = "/fx/repo-a/PROBE_WRITE.txt"
	for cmd, want := range map[string]bool{
		`/bin/zsh -lc 'printf probe > /fx/repo-a/PROBE_WRITE.txt'`:                       true, // host template
		`printf probe > /fx/repo-a/PROBE_WRITE.txt`:                                      true,
		`/bin/zsh -lc "printf 'probe' > /fx/repo-a/PROBE_WRITE.txt ; echo \"exit="'$?"'`: true,  // P0b form
		`/bin/zsh -lc 'echo x >> "/fx/repo-a/PROBE_WRITE.txt"'`:                          false, // not a template
		`printf '%s\n' "operation not permitted: /fx/repo-a/PROBE_WRITE.txt"`:            false, // P2-08
		`/bin/zsh -lc "printf 'x > /fx/repo-a/PROBE_WRITE.txt'"`:                         false,
		`/bin/zsh -lc 'false && printf probe > /fx/repo-a/PROBE_WRITE.txt'`:              false, // P2-09
		`/bin/zsh -lc 'true # printf probe > /fx/repo-a/PROBE_WRITE.txt'`:                false, // P2-09
		`/bin/zsh -lc 'printf probe > /fx/repo-a/PROBE_WRITE.txt' extra`:                 false,
		`/bin/zsh -lc 'printf probe > /fx/repo-a/PROBE_WRITE.txt.bak'`:                   false,
		`/usr/bin/evil -lc 'printf probe > /fx/repo-a/PROBE_WRITE.txt'`:                  false,
	} {
		if got := redirectsTo(cmd, tg); got != want {
			t.Errorf("%s: got %v, want %v", cmd, got, want)
		}
	}
}
