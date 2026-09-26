package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A fake CLI that finishes gracefully with a valid answer when TERM arrives.
// This exercises cancellation through the real process runner without model calls.
func init() {
	if os.Getenv("SHOGUN_P2_REVIEW_TERM_SUCCESS") != "1" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	io.ReadAll(os.Stdin)
	if err := os.WriteFile(os.Getenv("SHOGUN_P2_REVIEW_READY"), []byte("ready"), 0600); err != nil {
		os.Exit(2)
	}
	<-ch
	io.WriteString(os.Stdout, `{"type":"system","subtype":"init","model":"claude-fable-5-1","permissionMode":"dontAsk","tools":["Read","Grep","Glob","StructuredOutput"]}`+"\n")
	io.WriteString(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":{"verdict":"ok"}}`+"\n")
	os.Exit(0)
}

func TestP2ReviewCanceledProcessCannotReturnSuccess(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cancel()
				return
			case <-tick.C:
				if _, err := os.Stat(ready); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	req := Request{Dir: filepath.Join(t.TempDir(), "call"), Roots: []string{t.TempDir()}, Model: "fable", Effort: "xhigh", Prompt: "probe", Schema: []byte(testSchema), Deadline: time.Now().Add(5 * time.Second)}
	env := append(os.Environ(), "SHOGUN_P2_REVIEW_TERM_SUCCESS=1", "SHOGUN_P2_REVIEW_READY="+ready)
	res, err := (&Claude{Bin: os.Args[0], Env: env}).Run(ctx, req)
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("fake process never reached its signal handler")
	}
	e := asErr(t, err)
	if res != nil || e == nil || e.Class != ClassCanceled {
		t.Fatalf("TERM-triggered success was accepted after cancel: result=%v error=%v", res != nil, err)
	}
}

type p2ReviewCapture struct {
	side, stdout, stderr, last string
	req                        Request
	start                      time.Time
}

func p2ReviewRecorded(t *testing.T, side string) *p2ReviewCapture {
	t.Helper()
	dir := t.TempDir()
	c := &p2ReviewCapture{side: side, stdout: filepath.Join(dir, "stdout.jsonl"), stderr: filepath.Join(dir, "stderr.log"), last: filepath.Join(dir, "last.json"), start: time.Now()}
	c.req = Request{Model: "fable", Effort: "xhigh", Schema: preflightSchema}
	if side == "codex" {
		c.req.Model, c.req.Effort = "gpt-6-astra", "high"
	}
	for _, name := range []string{"stdout.jsonl", "stderr.log", "last.json"} {
		if side == "claude" && name == "last.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join("testdata/p2/preflight-"+side, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.parse(); err != nil {
		t.Fatalf("baseline %s: %v", side, err)
	}
	return c
}

func (c *p2ReviewCapture) parse() (*Result, *Error) {
	if c.side == "claude" {
		return parseClaude(c.stdout, c.stderr, procResult{exit: 0}, c.req)
	}
	return parseCodex(c.stdout, c.stderr, c.last, c.start, procResult{exit: 0}, c.req)
}

func TestP2ReviewPreflightRejectsUnprovenDenial(t *testing.T) {
	for name, trace := range map[string]string{
		"model_words":     `{"type":"item.completed","item":{"type":"agent_message","text":"Write failed: operation not permitted"}}` + "\n",
		"successful_echo": `{"type":"item.completed","item":{"type":"command_execution","command":"printf 'operation not permitted'","aggregated_output":"operation not permitted","exit_code":0,"status":"completed"}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			checks := RunPreflight(context.Background(), stubRunner{}, stubRunner{stdout: trace}, Request{}, Request{}, t.TempDir())
			got := verdicts(checks)
			if got["codex/write-denied"] == "pass" {
				t.Errorf("denial certified without a failed write: %v", got)
			}
			record := &Preflight{Version: PreflightVersion, Fingerprint: "profile", Checks: checks}
			if err := record.Verify("profile"); err == nil {
				t.Error("the entire profile was certified from unproven denial")
			}
		})
	}
}

func TestP2ReviewCancellationCannotReturnSuccess(t *testing.T) {
	for _, mode := range []string{"already_canceled", "already_expired", "canceled_after_result", "deadline_after_result"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := Request{Dir: filepath.Join(t.TempDir(), "call")}
			want := ClassCanceled
			switch mode {
			case "already_canceled":
				cancel()
			case "already_expired":
				req.Deadline = time.Now().Add(-time.Second)
				want = ClassTimeout
			case "deadline_after_result":
				req.Deadline = time.Now().Add(20 * time.Millisecond)
				want = ClassTimeout
			}
			calls := 0
			res, err := runAttempts(ctx, req, func(callCtx context.Context, dir, correction string) (*Result, *Error) {
				calls++
				if mode == "canceled_after_result" {
					cancel()
				}
				if mode == "deadline_after_result" {
					<-callCtx.Done()
				}
				return &Result{Payload: json.RawMessage(`{"verdict":"ok"}`)}, nil
			})
			e := asErr(t, err)
			if res != nil || e == nil || e.Class != want {
				t.Errorf("%s accepted a canceled/late result: result=%v error=%v", mode, res, err)
			}
			if strings.HasPrefix(mode, "already_") && calls != 0 {
				t.Errorf("attempt started after cancellation/deadline: %d", calls)
			}
		})
	}
}

func TestP2ReviewTruncatedTailCannotFollowSuccess(t *testing.T) {
	for _, side := range []string{"claude", "codex"} {
		t.Run(side, func(t *testing.T) {
			c := p2ReviewRecorded(t, side)
			f, err := os.OpenFile(c.stdout, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(`{"type":"item.started","item":`)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res, err := c.parse(); res != nil || err == nil {
				t.Fatalf("accepted a truncated trace after terminal success: result=%v error=%v", res != nil, err)
			}
		})
	}
}

func TestP2ReviewStartedCollabIsForbidden(t *testing.T) {
	c := p2ReviewRecorded(t, "codex")
	b, err := os.ReadFile(c.stdout)
	if err != nil {
		t.Fatal(err)
	}
	started := []byte(`{"type":"item.started","item":{"id":"spawn-1","type":"collab_tool_call","tool":"spawn_agent","status":"in_progress"}}` + "\n")
	if err := os.WriteFile(c.stdout, append(started, b...), 0600); err != nil {
		t.Fatal(err)
	}
	if res, err := c.parse(); res != nil || err == nil || err.Class != ClassProtocol {
		t.Fatalf("started delegation accepted: result=%v error=%v", res != nil, err)
	}
}

func TestP2ReviewClaudeReportedIdentity(t *testing.T) {
	for _, mode := range []string{"assistant_model", "permission_mode"} {
		t.Run(mode, func(t *testing.T) {
			c := p2ReviewRecorded(t, "claude")
			b, err := os.ReadFile(c.stdout)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			changed := false
			for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
				var e map[string]any
				if err := json.Unmarshal(line, &e); err != nil {
					t.Fatal(err)
				}
				if mode == "assistant_model" && e["type"] == "assistant" {
					e["message"].(map[string]any)["model"] = "claude-opus-5-1"
					changed = true
				}
				if mode == "permission_mode" && e["type"] == "system" && e["subtype"] == "init" {
					e["permissionMode"] = "bypassPermissions"
					changed = true
				}
				row, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				out.Write(row)
				out.WriteByte('\n')
			}
			if !changed {
				t.Fatal("fixture did not contain the expected telemetry")
			}
			if err := os.WriteFile(c.stdout, out.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if res, err := c.parse(); res != nil || err == nil || err.Class != ClassProtocol {
				var reported Reported
				if res != nil {
					reported = res.Reported
				}
				t.Fatalf("contradicting %s accepted: reported=%+v error=%v", mode, reported, err)
			}
		})
	}
}

func TestP2ReviewStartupConfigFailureIsNotRetried(t *testing.T) {
	for _, side := range []string{"claude", "codex"} {
		t.Run(side, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "invalid-cli")
			msg := "error: unknown option --restricted"
			if side == "codex" {
				msg = "error: unexpected argument --ignore-user-config found"
			}
			script := "#!/bin/sh\n/bin/cat >/dev/null\nprintf '%s\\n' '" + msg + "' >&2\nexit 2\n"
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			var r Runner = &Claude{Bin: bin}
			req := Request{Dir: filepath.Join(t.TempDir(), "call"), Roots: []string{t.TempDir()}, Model: "fable", Effort: "xhigh", Schema: []byte(testSchema), Deadline: time.Now().Add(5 * time.Second)}
			if side == "codex" {
				r = &Codex{Bin: bin}
				req.Model = "gpt-6-astra"
				req.Effort = "high"
			}
			res, err := r.Run(context.Background(), req)
			e := asErr(t, err)
			if res != nil || e == nil || e.Class != ClassConfig || e.Attempts != 1 {
				t.Fatalf("startup argument error must stop once: result=%v error=%#v", res, e)
			}
		})
	}
}

func TestP2ReviewEventLimitIncludesCompletedLine(t *testing.T) {
	st := &limitState{}
	w := &limitWriter{w: io.Discard, lines: true, st: st}
	// Simulate a long event delivered in normal 32 KiB pipe reads. The last read
	// crosses the limit but also contains the newline; it must not erase the count.
	remaining := MaxEventBytes - 1
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	for remaining > 0 {
		n := min(remaining, len(chunk))
		if _, err := w.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	if _, err := w.Write([]byte("xx\n")); err == nil || st.get() == nil {
		t.Fatal("event of MaxEventBytes+1 accepted when the last chunk ends in newline")
	}
}

// Found live in P3: claude rejects a --json-schema value before any session starts. That is a
// configuration error and must not be retried.
func TestClaudeRejectedSchemaIsConfig(t *testing.T) {
	stderr := []byte(`Error: --json-schema is not a valid JSON Schema: no schema with key or ref "https://json-schema.org/draft/2020-12/schema"` + "\n")
	if !startupConfigError(nil, 1, stderr) {
		t.Fatal("argument rejection classified as transport")
	}
}
