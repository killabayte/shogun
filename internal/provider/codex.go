package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Codex drives `codex exec` (reviewer). Bin is an absolute path or a PATH name.
type Codex struct {
	Bin string
	Env []string // base environment; nil = os.Environ()
}

// CodexDisabledFeatures are passed as --disable; doctor checks the names against `codex features list`.
var CodexDisabledFeatures = []string{"apps", "browser_use", "browser_use_external", "computer_use", "image_generation", "multi_agent", "goals", "hooks"}

const codexRustLog = "RUST_LOG=codex_exec=info,codex_core=info"

// CodexArgs is the accepted argv (cli-compatibility §2) without the binary.
func CodexArgs(req Request, schemaPath, lastPath string) []string {
	args := []string{"exec", "--json", "--output-schema", schemaPath, "-o", lastPath,
		"-m", req.Model, "-c", "model_reasoning_effort=" + req.Effort, "-c", `approval_policy="never"`,
		"-s", "read-only", "--ephemeral", "--skip-git-repo-check", "-C", req.Roots[0], "--ignore-user-config"}
	for _, f := range CodexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	args = append(args, "-c", "mcp_servers={}", "-c", "plugins={}")
	if !req.Web {
		args = append(args, "-c", `web_search="disabled"`)
	}
	return append(args, "-c", "project_doc_max_bytes=0", "-c", "agents.enabled=false", "-")
}

func (c *Codex) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Roots) == 0 {
		return nil, &Error{Class: ClassConfig, Msg: "no roots"}
	}
	return runAttempts(ctx, req, func(ctx context.Context, dir, correction string) (*Result, *Error) {
		schemaPath, lastPath := filepath.Join(dir, "schema.json"), filepath.Join(dir, "last.json")
		if err := os.WriteFile(schemaPath, req.Schema, 0o600); err != nil {
			return nil, fail(ClassConfig, "%v", err)
		}
		// A stale -o file must never be mistaken for this attempt's answer.
		if _, err := os.Stat(lastPath); !errors.Is(err, fs.ErrNotExist) {
			return nil, fail(ClassProtocol, "output file %s already exists before the call", lastPath)
		}
		args := CodexArgs(req, schemaPath, lastPath)
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
		p := proc{bin: c.Bin, args: args, env: childEnv(base, codexRustLog), dir: req.Roots[0], stdin: prompt,
			stdoutPath: filepath.Join(dir, "stdout.jsonl"), stderrPath: filepath.Join(dir, "stderr.log")}
		start := time.Now()
		pr, err := run(ctx, p)
		if err != nil {
			return nil, fail(ClassConfig, "start %s: %v", c.Bin, err)
		}
		return parseCodex(p.stdoutPath, p.stderrPath, lastPath, start, pr, req)
	})
}

type codexEvent struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Usage   json.RawMessage `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"item"`
}

var (
	reSession   = regexp.MustCompile(`codex_exec: .*SessionConfiguredEvent \{.*`)
	reModel     = regexp.MustCompile(`\bmodel: "([^"]+)"`)
	reEffort    = regexp.MustCompile(`\breasoning_effort: Some\((\w+)\)`)
	reApproval  = regexp.MustCompile(`\bapproval_policy: (\w+)`)
	reProfile   = regexp.MustCompile(`\bpermission_profile: (.*?), active_permission_profile`)
	reToolCall  = regexp.MustCompile(`codex_core::stream_events_utils: ToolCall: (\S+)`)
	reDelegated = regexp.MustCompile(`inter_agent_communication`)
)

func parseCodex(stdoutPath, stderrPath, lastPath string, start time.Time, pr procResult, req Request) (*Result, *Error) {
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
		Requested: Reported{Model: req.Model, Effort: req.Effort, ApprovalPolicy: "Never", PermissionProfile: "read-only"},
		Reported:  Reported{Model: Unknown, Effort: Unknown, ApprovalPolicy: Unknown, PermissionProfile: Unknown},
	}

	// Delegation is a contract violation wherever it shows up, whatever else happened.
	for _, m := range reToolCall.FindAllSubmatch(stderr, -1) {
		if strings.HasPrefix(string(m[1]), "collaboration") {
			return nil, fail(ClassProtocol, "sub-agent tool call %s", m[1])
		}
	}
	if reDelegated.Match(stderr) {
		return nil, fail(ClassProtocol, "inter-agent communication in the trace")
	}

	completed, failure := false, ""
	for _, line := range lines {
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fail(ClassTransport, "malformed event: %v", err)
		}
		switch ev.Type {
		case "thread.started", "turn.started":
		case "item.started", "item.updated", "item.completed":
			// Delegation is forbidden as soon as it is attempted, not only when it completes.
			if ev.Item != nil && ev.Item.Type == "collab_tool_call" {
				return nil, fail(ClassProtocol, "collab_tool_call in %s", ev.Type)
			}
			if ev.Type == "item.completed" && ev.Item != nil && ev.Item.Type == "error" {
				res.Degraded = append(res.Degraded, "error item: "+ev.Item.Message)
			}
		case "turn.completed":
			completed, res.Usage = true, ev.Usage
		case "turn.failed":
			if ev.Error != nil {
				failure = ev.Error.Message
			}
		case "error":
			if failure == "" {
				failure = ev.Message
			}
		default:
			res.Degraded = append(res.Degraded, "unknown event "+ev.Type)
		}
	}
	if failure != "" {
		return nil, fail(classifyCodexFailure(failure), "%s", failure)
	}
	if !completed {
		return nil, fail(ClassTransport, "no turn.completed (exit %d): %s", pr.exit, firstLine(stderr))
	}
	if pr.exit != 0 {
		return nil, fail(ClassTransport, "exit %d after turn.completed", pr.exit)
	}
	if e := checkSession(stderr, req, res); e != nil {
		return nil, e
	}
	st, err := os.Stat(lastPath)
	if err != nil {
		return nil, fail(ClassPayload, "no output file: %v", err)
	}
	if st.ModTime().Before(start.Add(-time.Second)) {
		return nil, fail(ClassProtocol, "output file predates this attempt")
	}
	payload, err := os.ReadFile(lastPath)
	if err != nil {
		return nil, fail(ClassPayload, "%v", err)
	}
	if err := ValidatePayload(req.Schema, payload); err != nil {
		return nil, fail(ClassPayload, "%v", err)
	}
	res.Payload, res.Finish = json.RawMessage(payload), "completed"
	if pr.stray {
		res.Degraded = append(res.Degraded, "a descendant process kept the output open and was killed")
	}
	return res, nil
}

// checkSession reads the per-call SessionConfiguredEvent: without it the call is unverified and
// rejected; a model/effort/policy/profile other than requested is a contract violation.
func checkSession(stderr []byte, req Request, res *Result) *Error {
	line := reSession.Find(stderr)
	if line == nil {
		return fail(ClassProtocol, "no SessionConfiguredEvent in stderr: model, effort and policy are unverified")
	}
	get := func(re *regexp.Regexp) string {
		if m := re.FindSubmatch(line); m != nil {
			return string(m[1])
		}
		return Unknown
	}
	res.Reported = Reported{Model: get(reModel), Effort: strings.ToLower(get(reEffort)),
		ApprovalPolicy: get(reApproval), PermissionProfile: get(reProfile)}
	r := res.Reported
	switch {
	case r.Model == Unknown || r.Effort == Unknown || r.ApprovalPolicy == Unknown || r.PermissionProfile == Unknown:
		return fail(ClassProtocol, "SessionConfiguredEvent is incomplete: %+v", r)
	case r.Model != req.Model:
		return fail(ClassProtocol, "requested model %q, CLI reports %q", req.Model, r.Model)
	case r.Effort != req.Effort:
		return fail(ClassProtocol, "requested effort %q, CLI reports %q", req.Effort, r.Effort)
	case r.ApprovalPolicy != "Never":
		return fail(ClassProtocol, "approval policy %q, want Never", r.ApprovalPolicy)
	case !strings.Contains(r.PermissionProfile, "access: Read") || strings.Contains(r.PermissionProfile, "access: Write") ||
		!strings.Contains(r.PermissionProfile, "network: Restricted"):
		return fail(ClassProtocol, "permission profile is not read-only/network-restricted: %s", r.PermissionProfile)
	}
	return nil
}

func classifyCodexFailure(msg string) Class {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, `"status":429`) || strings.Contains(m, `"status": 429`) ||
		strings.Contains(m, "usage limit") || strings.Contains(m, "rate limit"):
		return ClassRateLimit
	case strings.Contains(m, "invalid_request_error") || strings.Contains(m, `"status":400`) || strings.Contains(m, `"status": 400`) ||
		strings.Contains(m, `"status":401`) || strings.Contains(m, `"status":403`) || strings.Contains(m, "not supported"):
		return ClassConfig
	}
	return ClassTransport
}
