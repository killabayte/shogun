package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Claude drives `claude -p` (planner). Bin is an absolute path or a PATH name.
type Claude struct {
	Bin string
	Env []string // base environment; nil = os.Environ()
}

// claudeAliases are the model aliases the CLI resolves itself (claude --help).
var claudeAliases = map[string]bool{"fable": true, "opus": true, "sonnet": true, "haiku": true}

// ClaudeArgs is the accepted argv (cli-compatibility §1) without the binary.
func ClaudeArgs(req Request) []string {
	tools, args := "Read,Grep,Glob", []string{}
	if req.Web {
		tools += ",WebFetch"
		args = append(args, "--allowedTools", "WebFetch")
	}
	args = append([]string{"-p", "--output-format", "stream-json", "--verbose",
		"--model", req.Model, "--effort", req.Effort, "--json-schema", string(req.Schema),
		"--tools=" + tools}, args...)
	args = append(args, "--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--restricted", "--safe-mode", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence")
	for _, r := range req.Roots[1:] {
		args = append(args, "--add-dir", r)
	}
	return args
}

func (c *Claude) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Roots) == 0 {
		return nil, &Error{Class: ClassConfig, Msg: "no roots"}
	}
	return runAttempts(ctx, req, func(ctx context.Context, dir, correction string) (*Result, *Error) {
		args := ClaudeArgs(req)
		if err := writeJSON(filepath.Join(dir, "argv.json"), append([]string{c.Bin}, args...)); err != nil {
			return nil, fail(ClassConfig, "%v", err)
		}
		prompt := req.Prompt
		if correction != "" {
			prompt += correctionNote(correction)
		}
		base := c.Env
		if base == nil {
			base = os.Environ()
		}
		p := proc{bin: c.Bin, args: args, env: childEnv(base), dir: req.Roots[0], stdin: prompt,
			stdoutPath: filepath.Join(dir, "stdout.jsonl"), stderrPath: filepath.Join(dir, "stderr.log")}
		pr, err := run(ctx, p)
		if err != nil {
			return nil, fail(ClassConfig, "start %s: %v", c.Bin, err)
		}
		return parseClaude(p.stdoutPath, p.stderrPath, pr, req)
	})
}

type claudeEvent struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	Model          string          `json:"model"`
	PermissionMode string          `json:"permissionMode"`
	Tools          []string        `json:"tools"`
	Message        json.RawMessage `json:"message"` // an object on assistant events, a string on some others
	RateLimitInfo  *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
	// result
	IsError           bool            `json:"is_error"`
	StopReason        string          `json:"stop_reason"`
	TerminalReason    string          `json:"terminal_reason"`
	APIErrorStatus    int             `json:"api_error_status"`
	Result            string          `json:"result"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	ModelUsage        json.RawMessage `json:"modelUsage"`
	PermissionDenials []struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

func parseClaude(stdoutPath, stderrPath string, pr procResult, req Request) (*Result, *Error) {
	if pr.limitErr != nil {
		return nil, fail(ClassTransport, "%v", pr.limitErr)
	}
	lines, truncated, err := readEvents(stdoutPath)
	if err != nil {
		return nil, fail(ClassTransport, "read stdout: %v", err)
	}
	stderr, _ := os.ReadFile(stderrPath)
	if truncated {
		return nil, fail(ClassTransport, "stream ended mid-event (exit %d)", pr.exit)
	}
	if startupConfigError(lines, pr.exit, stderr) {
		return nil, fail(ClassConfig, "CLI rejected its arguments: %s", firstLine(stderr))
	}
	res := &Result{
		Requested: Reported{Model: req.Model, Effort: req.Effort, ApprovalPolicy: "dontAsk", PermissionProfile: "restricted"},
		Reported:  Reported{Model: Unknown, Effort: Unknown, ApprovalPolicy: Unknown, PermissionProfile: Unknown},
	}
	var final *claudeEvent
	rateLimited := false
	allowed := map[string]bool{"Read": true, "Grep": true, "Glob": true, "StructuredOutput": true, "WebFetch": req.Web}
	for _, line := range lines {
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fail(ClassTransport, "malformed event: %v", err)
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				// Claude reports the model and permission mode; the sandbox profile is not reported
				// and stays unknown (it is certified by the preflight, not per call).
				res.Reported.Model, res.Reported.ApprovalPolicy = ev.Model, ev.PermissionMode
				if ev.PermissionMode != "dontAsk" {
					return nil, fail(ClassProtocol, "permission mode %q, want dontAsk", ev.PermissionMode)
				}
				for _, t := range ev.Tools {
					if !allowed[t] {
						return nil, fail(ClassProtocol, "tool %q is available to the model; the isolation profile allows only %s", t, keys(allowed))
					}
				}
			}
		case "rate_limit_event":
			if ev.RateLimitInfo != nil && ev.RateLimitInfo.Status == "rejected" {
				rateLimited = true
			}
		case "result":
			e := ev
			final = &e
		case "assistant":
			// The main answer must come from the init model; helper models appear only in modelUsage.
			var m struct {
				Model string `json:"model"`
			}
			json.Unmarshal(ev.Message, &m)
			// "<synthetic>" messages are written by the CLI itself (e.g. "You've hit your session
			// limit"), not by a model; the result event classifies them.
			if m.Model != "" && m.Model != "<synthetic>" && m.Model != res.Reported.Model {
				return nil, fail(ClassProtocol, "assistant message from %q, session model %q", m.Model, res.Reported.Model)
			}
		case "user":
		default:
			res.Degraded = append(res.Degraded, "unknown event "+ev.Type)
		}
	}
	if final == nil {
		if rateLimited {
			return nil, fail(ClassRateLimit, "rate limit rejected before a result")
		}
		return nil, fail(ClassTransport, "no result event (exit %d): %s", pr.exit, firstLine(stderr))
	}
	res.Finish, res.Usage = final.TerminalReason, final.ModelUsage
	// Failures after the result event still carry the usage it reported.
	fail := func(c Class, format string, a ...any) *Error {
		e := fail(c, format, a...)
		e.Usage = final.ModelUsage
		return e
	}
	for _, d := range final.PermissionDenials {
		res.Degraded = append(res.Degraded, fmt.Sprintf("permission denied: %s %s", d.ToolName, d.ToolInput))
	}
	switch {
	case rateLimited || final.APIErrorStatus == 429:
		return nil, fail(ClassRateLimit, "%s", final.Result)
	case final.StopReason == "refusal":
		return nil, fail(ClassRefusal, "%s", final.Result)
	case final.IsError && (final.APIErrorStatus == 401 || final.APIErrorStatus == 403 || final.APIErrorStatus == 404 ||
		strings.Contains(string(stderr), "unrecognized_model")):
		return nil, fail(ClassConfig, "%s", strings.TrimSpace(final.Result+" "+firstLine(stderr)))
	case strings.HasPrefix(final.Subtype, "error_max_structured_output"):
		return nil, fail(ClassPayload, "structured output retries exhausted: %s", final.Subtype)
	case final.IsError || final.Subtype != "success" || pr.exit != 0:
		return nil, fail(ClassTransport, "subtype=%s is_error=%v exit=%d: %s", final.Subtype, final.IsError, pr.exit, final.Result)
	}
	if !claudeModelMatches(req.Model, res.Reported.Model) {
		return nil, fail(ClassProtocol, "requested model %q, CLI reports %q", req.Model, res.Reported.Model)
	}
	if len(final.StructuredOutput) == 0 || string(final.StructuredOutput) == "null" {
		return nil, fail(ClassPayload, "result has no structured_output")
	}
	if err := ValidatePayload(req.Schema, final.StructuredOutput); err != nil {
		return nil, fail(ClassPayload, "%v", err)
	}
	res.Payload = final.StructuredOutput
	if pr.stray {
		res.Degraded = append(res.Degraded, "a descendant process kept the output open and was killed")
	}
	return res, nil
}

// claudeModelMatches: an alias matches its family ("fable" ~ "claude-fable-5-1"), an id matches exactly.
func claudeModelMatches(requested, reported string) bool {
	if claudeAliases[requested] {
		return strings.HasPrefix(reported, "claude-"+requested+"-")
	}
	return requested == reported
}

func keys(m map[string]bool) string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
