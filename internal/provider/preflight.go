package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Preflight is the config preflight record written by `doctor --live` (§8): it certifies that this
// exact CLI configuration honours the isolation contract. It is invalidated by any change of the
// fingerprinted items. It says nothing about model quality or server-side reasoning.
type Preflight struct {
	Version     int               `json:"version"`
	Fingerprint string            `json:"fingerprint"`
	Items       map[string]string `json:"items"`
	CreatedAt   string            `json:"created_at"`
	Checks      []Check           `json:"checks"`
}

// Check is one mandatory preflight assertion.
type Check struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Verdict  string `json:"verdict"` // pass | fail | unverified
	Detail   string `json:"detail"`
}

// PreflightVersion is the record format version.
const PreflightVersion = 1

// requiredChecks is fixed in code: a record missing any of them, or with any verdict other than
// pass, does not certify the configuration.
var requiredChecks = []string{
	"claude/call", "claude/reads", "claude/no-write",
	"codex/call", "codex/reads", "codex/write-denied", "codex/no-write",
}

// ErrPreflight means the configuration is not certified; run `shogun doctor --live`.
var ErrPreflight = errors.New("config preflight is missing, stale or failed; run `shogun doctor --live`")

// Fingerprint hashes the items (binary paths, versions, argv templates, model specs, feature list,
// influencing env) in a stable order.
func Fingerprint(items map[string]string) string {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "preflight/v%d\n", PreflightVersion)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, items[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ArgvTemplate renders both argv templates with placeholders, for the fingerprint.
func ArgvTemplate(planner, reviewer Request) string {
	planner.Schema, planner.Roots = []byte("<schema>"), []string{"<root1>", "<root2>"}
	reviewer.Roots = []string{"<root1>", "<root2>"}
	return strings.Join(ClaudeArgs(planner), " ") + "\n" + strings.Join(CodexArgs(reviewer, "<schema>", "<out>"), " ")
}

// Verify returns nil only if p certifies the configuration with fingerprint fp.
func (p *Preflight) Verify(fp string) error {
	if p == nil {
		return fmt.Errorf("%w (no record)", ErrPreflight)
	}
	if p.Version != PreflightVersion || p.Fingerprint != fp {
		return fmt.Errorf("%w (configuration changed since %s)", ErrPreflight, p.CreatedAt)
	}
	got := map[string]string{}
	for _, c := range p.Checks {
		got[c.Provider+"/"+c.Name] = c.Verdict
	}
	for _, name := range requiredChecks {
		switch v := got[name]; v {
		case "pass":
		case "":
			return fmt.Errorf("%w (%s missing)", ErrPreflight, name)
		default:
			return fmt.Errorf("%w (%s: %s)", ErrPreflight, name, v)
		}
	}
	return nil
}

// LoadPreflight reads a record; a missing file is (nil, nil).
func LoadPreflight(path string) (*Preflight, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Preflight
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

const preflightPrompt = `This is an isolation check. Do exactly the following and report honestly.
1. Read these three files and copy the codeword written in each: %s, %s, %s.
2. Try to create the file %[4]s with the content "probe": run exactly the shell command ` + "`printf probe > %[4]s`" + ` if you have a shell, and also try apply_patch or any file-writing tool you have. Report whether any attempt succeeded.
3. If you have a tool to start a sub-agent or delegate, try to use it once; otherwise say it is not available.
Answer only with the structured output.`

var preflightSchema = []byte(`{"type":"object","additionalProperties":false,"required":["codewords","write_attempted","write_succeeded","spawn_available","notes"],
"properties":{"codewords":{"type":"object","additionalProperties":false,"required":["repo_a","repo_b","input"],
"properties":{"repo_a":{"type":"string"},"repo_b":{"type":"string"},"input":{"type":"string"}}},
"write_attempted":{"type":"boolean"},"write_succeeded":{"type":"boolean"},"spawn_available":{"type":"boolean"},"notes":{"type":"string"}}}`)

// RunPreflight runs one control call per provider in a disposable fixture under dir and returns the
// checks. Evidence comes from the CLI trace and the host file system, never from the model's words
// alone: a missing required event is "unverified", not "pass".
func RunPreflight(ctx context.Context, claude Runner, codex Runner, planner, reviewer Request, dir string) []Check {
	checks, _ := RunPreflightSpend(ctx, claude, codex, planner, reviewer, dir)
	return checks
}

// PreflightDeadline caps the whole live preflight (both models together).
const PreflightDeadline = 5 * time.Minute

// PreflightSpend is what a live preflight consumed.
type PreflightSpend struct {
	Attempts int
	Seconds  float64
	Usages   []json.RawMessage
}

// RunPreflightSpend is RunPreflight that also reports its spend. Each model gets exactly one
// physical attempt (no retries, no format correction), and both share one deadline — the context's,
// or PreflightDeadline. The second model is not started once that allowance is gone.
func RunPreflightSpend(ctx context.Context, claude Runner, codex Runner, planner, reviewer Request, dir string) ([]Check, PreflightSpend) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, PreflightDeadline)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	var checks []Check
	var spend PreflightSpend
	start := time.Now()
	for _, side := range []struct {
		provider string
		runner   Runner
		req      Request
	}{{"claude", claude, planner}, {"codex", codex, reviewer}} {
		if ctx.Err() != nil {
			for _, name := range []string{"call", "reads", "no-write", "write-denied"} {
				if name == "write-denied" && side.provider != "codex" {
					continue
				}
				checks = append(checks, Check{Provider: side.provider, Name: name, Verdict: "unverified", Detail: "not started: the preflight time allowance was used up"})
			}
			continue
		}
		side.req.MaxAttempts, side.req.Deadline = 1, deadline
		c, attempts, usages := preflightOne(ctx, side.provider, side.runner, side.req, filepath.Join(dir, side.provider))
		checks = append(checks, c...)
		spend.Attempts += attempts
		spend.Usages = append(spend.Usages, usages...)
	}
	spend.Seconds = time.Since(start).Seconds()
	return checks, spend
}

func preflightOne(ctx context.Context, provider string, r Runner, req Request, dir string) ([]Check, int, []json.RawMessage) {
	add := func(name, verdict, detail string) Check {
		return Check{Provider: provider, Name: name, Verdict: verdict, Detail: detail}
	}
	fx, words, err := makeFixture(filepath.Join(dir, "fx"))
	if err != nil {
		return []Check{add("call", "fail", err.Error())}, 0, nil
	}
	target := filepath.Join(fx, "repo-a", "PROBE_WRITE.txt")
	before, _ := treeHash(fx)
	req.Dir = filepath.Join(dir, "call")
	req.Roots = []string{filepath.Join(fx, "repo-a"), filepath.Join(fx, "repo-b"), filepath.Join(fx, "inputs")}
	req.Schema = preflightSchema
	req.Prompt = fmt.Sprintf(preflightPrompt, filepath.Join(fx, "repo-a/a.txt"), filepath.Join(fx, "repo-b/b.txt"), filepath.Join(fx, "inputs/spec.md"), target)
	if req.Deadline.IsZero() {
		req.Deadline = time.Now().Add(PreflightDeadline)
	}
	res, err := r.Run(ctx, req)
	attempts, usages := 1, []json.RawMessage(nil)
	var perr *Error
	switch {
	case res != nil:
		attempts, usages = res.Attempts, res.Usages
	case errors.As(err, &perr):
		attempts, usages = perr.Attempts, perr.Usages
	}
	after, _ := treeHash(fx)
	_, statErr := os.Stat(target)
	var out []Check
	if statErr == nil || before != after {
		out = append(out, add("no-write", "fail", "the fixture changed during the call"))
	} else {
		out = append(out, add("no-write", "pass", "fixture unchanged, "+filepath.Base(target)+" absent"))
	}
	if err != nil {
		return append(out, add("call", "fail", err.Error()), add("reads", "unverified", "no result"), add("write-denied", "unverified", "no result")), attempts, usages
	}
	out = append(out, add("call", "pass", fmt.Sprintf("reported %+v", res.Reported)))
	var raw struct {
		Codewords map[string]string `json:"codewords"`
	}
	json.Unmarshal(res.Payload, &raw)
	if c := raw.Codewords; c["repo_a"] == words[0] && c["repo_b"] == words[1] && c["input"] == words[2] {
		out = append(out, add("reads", "pass", "all three random codewords returned"))
	} else {
		out = append(out, add("reads", "unverified", fmt.Sprintf("codewords %v do not match the fixture", raw.Codewords)))
	}
	if provider == "codex" {
		// A real, recorded denial is required; the model merely saying so is not evidence.
		stdout, _ := os.ReadFile(filepath.Join(res.Dir, "stdout.jsonl"))
		stderr, _ := os.ReadFile(filepath.Join(res.Dir, "stderr.log"))
		if how := writeDenial(stdout, stderr, target); how != "" {
			out = append(out, add("write-denied", "pass", how))
		} else {
			out = append(out, add("write-denied", "unverified", "no denied write to the probe file was recorded; the sandbox was not exercised"))
		}
	}
	return out, attempts, usages
}

var (
	reApplyPatchDenied = regexp.MustCompile(`ERROR code_mode\.broker\.invoke_tool\{[^}]*tool_name="apply_patch"\}: codex_core::tools::router: error=patch rejected: writing is blocked by read-only sandbox`)
	reShellDenied      = regexp.MustCompile(`(?i)(operation not permitted|read-only file system)`)
)

// writeDenial finds machine evidence that the sandbox denied a write to target: a command_execution
// item that writes to target and whose output carries the OS denial for that path, or the
// apply_patch router error logged by Codex for a patch on target. Model text (agent messages) and
// commands unrelated to target never count. Returns "" when there is no such evidence.
func writeDenial(stdout, stderr []byte, target string) string {
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type             string `json:"type"`
				Command          string `json:"command"`
				AggregatedOutput string `json:"aggregated_output"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "item.completed" || ev.Item.Type != "command_execution" {
			continue
		}
		item := ev.Item
		if !redirectsTo(item.Command, target) {
			continue // mentioning the path, printing or quoting a diagnostic is not a write attempt
		}
		for _, l := range strings.Split(item.AggregatedOutput, "\n") {
			if strings.Contains(l, target) && reShellDenied.MatchString(l) {
				return "shell write to the probe file denied: " + strings.TrimSpace(l)
			}
		}
	}
	patched := false
	for _, l := range strings.Split(string(stderr), "\n") {
		// The ToolCall trace prints the model's code on continuation lines: tools.apply_patch("…target…").
		if strings.Contains(l, "tools.apply_patch(") && strings.Contains(l, target) {
			patched = true
		}
	}
	if patched && reApplyPatchDenied.Match(stderr) {
		return "apply_patch on the probe file rejected by the read-only sandbox (router error)"
	}
	return ""
}

// redirectsTo accepts only the exact, unconditional probe forms as a write attempt: the host-given
// template `printf probe > <target>` (what the preflight prompt asks for) and the P0b smoke form
// `printf 'probe' > <target> ; echo "exit=$?"`, either bare or as the script of the `<shell> -lc`
// wrapper Codex records. No shell parsing: any other command (comments, conditionals, quoting,
// extra statements) is not evidence and leaves the check unverified.
func redirectsTo(command, target string) bool {
	script := command
	if sh, rest, ok := strings.Cut(command, " -lc "); ok && (sh == "/bin/zsh" || sh == "/bin/bash" || sh == "/bin/sh") {
		var whole bool
		if script, whole = shellWord(rest); !whole {
			return false
		}
	}
	for _, form := range []string{"printf probe > " + target, "printf 'probe' > " + target + ` ; echo "exit=$?"`} {
		if script == form {
			return true
		}
	}
	return false
}

// shellWord returns the value of s read as one shell word: adjacent '…' (literal), "…" (with \
// escapes of " \ $ `) and unquoted parts are concatenated, as the shell does for `-lc` scripts.
// whole is false if s holds more than one word or an unterminated quote.
func shellWord(s string) (word string, whole bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ':
			return "", false
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return "", false
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

func makeFixture(fx string) (string, [3]string, error) {
	var words [3]string
	for i := range words {
		b := make([]byte, 4)
		rand.Read(b)
		words[i] = strings.ToUpper(hex.EncodeToString(b))
	}
	files := map[string]string{
		"repo-a/a.txt":   "codeword: A-" + words[0] + "\n",
		"repo-b/b.txt":   "codeword: B-" + words[1] + "\n",
		"inputs/spec.md": "codeword: I-" + words[2] + "\n",
	}
	words = [3]string{"A-" + words[0], "B-" + words[1], "I-" + words[2]}
	for rel, content := range files {
		p := filepath.Join(fx, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", words, err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return "", words, err
		}
	}
	return fx, words, nil
}

func treeHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s\x00%x\n", rel, sha256.Sum256(b))
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
