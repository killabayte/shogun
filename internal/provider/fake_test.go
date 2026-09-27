package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake claude/codex executable: with SHOGUN_FAKE_CLI set it replays
// the protocol for the scenario of the current attempt (FAKE_SCRIPT=a,b,c — one per attempt, the
// last one repeats) and records every prompt in FAKE_RECORD. No model is ever called.
func TestMain(m *testing.M) {
	switch os.Getenv("SHOGUN_FAKE_CLI") {
	case "":
		os.Exit(m.Run())
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	default:
		os.Exit(fakeCLI(os.Args[1:]))
	}
}

func fakeCLI(args []string) int {
	prompt, _ := io.ReadAll(os.Stdin) // blocks forever unless the adapter closes stdin (EOF)
	rec := os.Getenv("FAKE_RECORD")
	n := 1
	if b, err := os.ReadFile(filepath.Join(rec, "count")); err == nil {
		n, _ = strconv.Atoi(string(b))
		n++
	}
	os.WriteFile(filepath.Join(rec, "count"), []byte(strconv.Itoa(n)), 0o600)
	os.WriteFile(filepath.Join(rec, fmt.Sprintf("prompt-%d.txt", n)), prompt, 0o600)
	os.WriteFile(filepath.Join(rec, fmt.Sprintf("env-%d.txt", n)), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	script := strings.Split(os.Getenv("FAKE_SCRIPT"), ",")
	sc := script[min(n, len(script))-1]
	payload := os.Getenv("FAKE_PAYLOAD")
	if payload == "" {
		payload = `{"verdict":"ok"}`
	}
	out := func(v any) { b, _ := json.Marshal(v); os.Stdout.Write(append(b, '\n')) }
	arg := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}

	switch sc {
	case "hang": // ignores TERM: only the KILL escalation stops it
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		return 0
	case "child": // a descendant keeps stdout open after the leader exits
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SHOGUN_FAKE_CLI=sleep")
		c.Stdout = os.Stdout
		c.Start()
		sc = "ok"
	case "huge":
		os.Stdout.WriteString(`{"type":"x","pad":"` + strings.Repeat("a", MaxEventBytes+1) + "\"}\n")
		return 0
	}

	if len(args) > 0 && args[0] == "exec" { // codex
		model, effort := arg("-m"), strings.TrimPrefix(arg("-c"), "model_reasoning_effort=")
		if sc == "wrongeffort" {
			effort = "low"
		}
		if sc != "nosession" {
			fmt.Fprintf(os.Stderr, `2026-09-26T00:00:00Z  INFO codex.exec{}: codex_exec: Codex initialized with event: SessionConfiguredEvent { model: "%s", model_provider_id: "openai", approval_policy: Never, permission_profile: Managed { file_system: Restricted { entries: [FileSystemSandboxEntry { path: Special { value: Root }, access: Read }] }, network: Restricted }, active_permission_profile: None, reasoning_effort: Some(%s) }`+"\n",
				model, strings.ToUpper(effort[:1])+effort[1:])
		}
		out(map[string]any{"type": "thread.started", "thread_id": "t"})
		out(map[string]any{"type": "turn.started"})
		switch sc {
		case "spawn":
			fmt.Fprintln(os.Stderr, `INFO turn{}: codex_core::stream_events_utils: ToolCall: collaborationspawn_agent {}`)
		case "collab":
			out(map[string]any{"type": "item.completed", "item": map[string]any{"type": "collab_tool_call"}})
		case "ratelimit":
			out(map[string]any{"type": "error", "message": `{"type":"error","status":429,"error":{"message":"usage limit reached"}}`})
			out(map[string]any{"type": "turn.failed", "error": map[string]any{"message": `{"status":429}`}})
			return 1
		case "crash":
			return 1
		case "unknown":
			out(map[string]any{"type": "mystery.event"})
		case "badpayload":
			payload = `{"wrong":1}`
		}
		out(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": payload}})
		out(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		if sc != "noout" {
			os.WriteFile(arg("-o"), []byte(payload), 0o600)
		}
		return 0
	}

	// claude
	tools := []string{"Glob", "Grep", "Read", "StructuredOutput"}
	if sc == "writetool" {
		tools = append(tools, "Bash")
	}
	out(map[string]any{"type": "system", "subtype": "init", "model": "claude-fable-5-1", "permissionMode": "dontAsk", "tools": tools})
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "terminal_reason": "completed",
		"structured_output": json.RawMessage(payload), "modelUsage": map[string]any{"claude-fable-5-1": map[string]any{"outputTokens": 1}},
		"permission_denials": []any{}}
	switch sc {
	case "crash":
		return 1
	case "truncated":
		os.Stdout.WriteString(`{"type":"assistant","message":{"content":[{"type":"te`)
		return 0
	case "ratelimit":
		out(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected"}})
		result["is_error"], result["api_error_status"], result["result"] = true, 429, "limit"
	case "refusal":
		result["stop_reason"], result["structured_output"] = "refusal", nil
	case "badpayload":
		result["structured_output"] = json.RawMessage(`{"wrong":1}`)
	case "denied":
		result["permission_denials"] = []any{map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": "/elsewhere"}}}
	case "unknown":
		out(map[string]any{"type": "mystery"})
	case "big":
		out(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("b", 200<<10)}}}})
	}
	out(result)
	return 0
}
