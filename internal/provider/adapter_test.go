package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSchema = `{"type":"object","additionalProperties":false,"required":["verdict"],"properties":{"verdict":{"type":"string"}}}`

type fake struct {
	t   *testing.T
	rec string
	env []string
}

func newFake(t *testing.T, script string, extra ...string) *fake {
	rec := t.TempDir()
	env := append(os.Environ(), "SHOGUN_FAKE_CLI=1", "FAKE_SCRIPT="+script, "FAKE_RECORD="+rec,
		"ANTHROPIC_API_KEY=secret", "OPENAI_API_KEY=secret", "CLAUDECODE=1", "ANTHROPIC_MODEL=override")
	return &fake{t: t, rec: rec, env: append(env, extra...)}
}

func (f *fake) attempts() int {
	b, _ := os.ReadFile(filepath.Join(f.rec, "count"))
	n := 0
	for _, c := range string(b) {
		n = n*10 + int(c-'0')
	}
	return n
}

func (f *fake) request(model, effort string) Request {
	return Request{Dir: filepath.Join(f.t.TempDir(), "call"), Model: model, Effort: effort, Prompt: "PROMPT-TEXT",
		Schema: []byte(testSchema), Roots: []string{f.t.TempDir(), f.t.TempDir()}, Deadline: time.Now().Add(30 * time.Second)}
}

func runClaude(t *testing.T, script string) (*Result, *Error, *fake) {
	f := newFake(t, script)
	res, err := (&Claude{Bin: os.Args[0], Env: f.env}).Run(context.Background(), f.request("fable", "xhigh"))
	return res, asErr(t, err), f
}

func runCodex(t *testing.T, script string) (*Result, *Error, *fake) {
	f := newFake(t, script)
	res, err := (&Codex{Bin: os.Args[0], Env: f.env}).Run(context.Background(), f.request("gpt-6-astra", "high"))
	return res, asErr(t, err), f
}

func asErr(t *testing.T, err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("not a provider error: %v", err)
	}
	return e
}

func TestAdaptersSuccessAndEnvironment(t *testing.T) {
	for name, run := range map[string]func(*testing.T, string) (*Result, *Error, *fake){"claude": runClaude, "codex": runCodex} {
		t.Run(name, func(t *testing.T) {
			res, err, f := run(t, "ok")
			if err != nil {
				t.Fatal(err)
			}
			if string(res.Payload) != `{"verdict":"ok"}` || res.Attempts != 1 || f.attempts() != 1 {
				t.Fatalf("result %+v", res)
			}
			if p, _ := os.ReadFile(filepath.Join(f.rec, "prompt-1.txt")); string(p) != "PROMPT-TEXT" {
				t.Fatalf("prompt not delivered over stdin with EOF: %q", p)
			}
			env, _ := os.ReadFile(filepath.Join(f.rec, "env-1.txt"))
			for _, banned := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "CLAUDECODE=", "ANTHROPIC_MODEL="} {
				if strings.Contains(string(env), banned) {
					t.Errorf("%s leaked into the child environment", banned)
				}
			}
			for _, f := range []string{"argv.json", "stdout.jsonl", "stderr.log"} {
				if _, err := os.Stat(filepath.Join(res.Dir, f)); err != nil {
					t.Errorf("attempt artifact %s: %v", f, err)
				}
			}
		})
	}
}

func TestClaudeFailures(t *testing.T) {
	for _, tc := range []struct {
		script   string
		class    Class
		attempts int
	}{
		{"crash", ClassTransport, 3},
		{"truncated", ClassTransport, 3},
		{"huge", ClassTransport, 3},
		{"ratelimit", ClassRateLimit, 1},
		{"refusal", ClassRefusal, 1},
		{"writetool", ClassProtocol, 1},
		{"badpayload", ClassPayload, 2},
	} {
		t.Run(tc.script, func(t *testing.T) {
			res, err, f := runClaude(t, tc.script)
			if err == nil {
				t.Fatalf("accepted: %+v", res)
			}
			if err.Class != tc.class || err.Attempts != tc.attempts || f.attempts() != tc.attempts {
				t.Fatalf("got %s after %d attempts (fake saw %d), want %s after %d: %s", err.Class, err.Attempts, f.attempts(), tc.class, tc.attempts, err.Msg)
			}
		})
	}
}

func TestCodexFailures(t *testing.T) {
	for _, tc := range []struct {
		script   string
		class    Class
		attempts int
	}{
		{"crash", ClassTransport, 3},
		{"ratelimit", ClassRateLimit, 1},
		{"nosession", ClassProtocol, 1},
		{"wrongeffort", ClassProtocol, 1},
		{"spawn", ClassProtocol, 1},
		{"collab", ClassProtocol, 1},
		{"noout", ClassPayload, 2},
		{"badpayload", ClassPayload, 2},
	} {
		t.Run(tc.script, func(t *testing.T) {
			res, err, f := runCodex(t, tc.script)
			if err == nil {
				t.Fatalf("accepted: %+v", res)
			}
			if err.Class != tc.class || err.Attempts != tc.attempts || f.attempts() != tc.attempts {
				t.Fatalf("got %s after %d attempts (fake saw %d), want %s after %d: %s", err.Class, err.Attempts, f.attempts(), tc.class, tc.attempts, err.Msg)
			}
		})
	}
}

// Transport failure is retried; a payload failure gets exactly one correction, which carries the error.
func TestRetryAndCorrection(t *testing.T) {
	res, err, f := runClaude(t, "crash,ok")
	if err != nil || res.Attempts != 2 || f.attempts() != 2 {
		t.Fatalf("transport retry: %+v %v", res, err)
	}
	res, err, f = runCodex(t, "badpayload,ok")
	if err != nil || res.Attempts != 2 {
		t.Fatalf("correction: %+v %v", res, err)
	}
	p, _ := os.ReadFile(filepath.Join(f.rec, "prompt-2.txt"))
	if !strings.HasPrefix(string(p), "PROMPT-TEXT") || !strings.Contains(string(p), "rejected by the output contract") {
		t.Fatalf("correction prompt: %q", p)
	}
}

func TestNonFatalSignalsAreDegradedNotFailures(t *testing.T) {
	for _, tc := range []struct {
		run    func(*testing.T, string) (*Result, *Error, *fake)
		script string
		want   string
	}{
		{runClaude, "denied", "permission denied: Read"},
		{runClaude, "unknown", "unknown event mystery"},
		{runCodex, "unknown", "unknown event mystery.event"},
		{runClaude, "big", ""}, // one 200 KiB event: above bufio.Scanner's 64 KiB default
	} {
		res, err, _ := tc.run(t, tc.script)
		if err != nil {
			t.Fatalf("%s: %v", tc.script, err)
		}
		if tc.want != "" && !strings.Contains(strings.Join(res.Degraded, "\n"), tc.want) {
			t.Errorf("%s: degraded %v, want %q", tc.script, res.Degraded, tc.want)
		}
	}
}

func TestHungProcessIsKilledOnDeadlineAndCancel(t *testing.T) {
	old := killGrace
	killGrace = 200 * time.Millisecond
	defer func() { killGrace = old }()

	f := newFake(t, "hang")
	req := f.request("fable", "xhigh")
	req.Deadline = time.Now().Add(300 * time.Millisecond)
	start := time.Now()
	_, err := (&Claude{Bin: os.Args[0], Env: f.env}).Run(context.Background(), req)
	if e := asErr(t, err); e == nil || e.Class != ClassTimeout || e.Attempts != 1 {
		t.Fatalf("deadline: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("hung process was not killed promptly: %s", d)
	}

	f = newFake(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err = (&Codex{Bin: os.Args[0], Env: f.env}).Run(ctx, f.request("gpt-6-astra", "high"))
	if e := asErr(t, err); e == nil || e.Class != ClassCanceled || e.Attempts != 1 {
		t.Fatalf("cancel: %v", err)
	}
}

// A descendant that keeps stdout open must not hang the call; it is killed and noted.
func TestStrayDescendantDoesNotHang(t *testing.T) {
	start := time.Now()
	res, err, _ := runClaude(t, "child")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || !strings.Contains(strings.Join(res.Degraded, "\n"), "descendant") {
		t.Fatalf("took %s, degraded %v", time.Since(start), res.Degraded)
	}
}

func TestArgvMatchesContract(t *testing.T) {
	req := Request{Model: "fable", Effort: "xhigh", Schema: []byte(`{}`), Roots: []string{"/r1", "/r2", "/r3"}}
	got := strings.Join(ClaudeArgs(req), " ")
	for _, want := range []string{"-p --output-format stream-json --verbose --model fable --effort xhigh --json-schema {}",
		"--tools=Read,Grep,Glob --permission-mode dontAsk --permission-prompts none --restricted --safe-mode --strict-mcp-config --disable-slash-commands --no-session-persistence --add-dir /r2 --add-dir /r3"} {
		if !strings.Contains(got, want) {
			t.Errorf("claude argv %q lacks %q", got, want)
		}
	}
	req.Web = true
	if got := strings.Join(ClaudeArgs(req), " "); !strings.Contains(got, "--tools=Read,Grep,Glob,WebFetch --allowedTools WebFetch") {
		t.Errorf("claude web argv: %s", got)
	}

	req = Request{Model: "gpt-6-astra", Effort: "high", Roots: []string{"/r1", "/r2"}}
	want, _ := os.ReadFile("../../scripts/p0a/codex-final-full-agentsoff.argv.json")
	var accepted []string
	if err := json.Unmarshal(want, &accepted); err != nil {
		t.Fatal(err)
	}
	gotArgs := CodexArgs(req, "S", "O")
	// The accepted smoke argv uses placeholders and a different option order; compare as sets of
	// option/value pairs so the contract is checked, not the order.
	if a, b := pairs(gotArgs), pairs(accepted); !sameKeys(a, b, "--output-schema", "-o", "-C") {
		t.Errorf("codex argv differs from the accepted smoke profile:\n got %v\nwant %v", gotArgs, accepted)
	}
	if strings.Contains(strings.Join(gotArgs, " "), "--add-dir") {
		t.Error("codex --add-dir grants write access and must not be used")
	}
	req.Web = true
	if strings.Contains(strings.Join(CodexArgs(req, "S", "O"), " "), "web_search") {
		t.Error("research keeps web search")
	}
}

func pairs(args []string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out[a+" "+args[i+1]] = true
			i++
			continue
		}
		out[a] = true
	}
	return out
}

// sameKeys compares pair sets, ignoring the values of options whose value is a path.
func sameKeys(a, b map[string]bool, pathOpts ...string) bool {
	norm := func(m map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k := range m {
			for _, p := range pathOpts {
				if strings.HasPrefix(k, p+" ") {
					k = p
				}
			}
			out[k] = true
		}
		return out
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for k := range na {
		if !nb[k] {
			return false
		}
	}
	return true
}

// The usage of a failed attempt (here: an invalid payload before the correction) is kept.
func TestUsageOfEveryAttemptIsKept(t *testing.T) {
	res, err, _ := runClaude(t, "badpayload,ok")
	if err != nil || res.Attempts != 2 || len(res.Usages) != 2 || len(res.Usages[0]) == 0 || len(res.Usages[1]) == 0 {
		t.Fatalf("usages %v, err %v", res, err)
	}
	_, e, _ := runClaude(t, "badpayload")
	if e == nil || len(e.Usages) != 2 || len(e.Usages[0]) == 0 {
		t.Fatalf("usage of failed attempts lost: %+v", e)
	}
}
