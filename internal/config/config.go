package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the effective, validated configuration.
type Config struct {
	Planner  ModelSpec
	Reviewer ModelSpec

	ClaudeCommand string // executable path or name for exec.LookPath
	CodexCommand  string

	PlansDir string // "" = library disabled, plans go to <workspace>/docs/plans
	Project  string // "" = derived from workspace basename
	Lang     string // "" = language of the task

	ReviewRounds     int           // R: max review rounds per unit
	DetailBatch      int           // k: steps per detail call
	CallDeadline     time.Duration // hard deadline per logical call
	MaxCalls         int           // 0 = derived from outline (2R(B+3)); counts physical attempts
	MaxTime          time.Duration // 0 = derived
	MaxContextTokens int           // heuristic threshold before integration

	// Jev (docs/plans/shogun-jev.md): an advisory classifier, off by default. JevKeyEnv names the
	// environment variable that holds the key; the key itself is never a config value.
	Jev       string // "off" | "advisory"
	JevKeyEnv string // default TYPESAFE_API_KEY
	JevModel  string // pinned model id; aliases move on their own
}

// Jev modes and defaults (kept in sync with internal/jev by a test).
const (
	JevOff        = "off"
	JevAdvisory   = "advisory"
	JevDefaultEnv = "TYPESAFE_API_KEY"
	JevDefault    = "jev-1.13.0"
)

// Source tells where an effective value came from.
type Source string

// Loaded is the result of Load: effective config plus provenance per key.
type Loaded struct {
	Config     Config
	Provenance map[string]Source
	Files      []string // config files that were read, in precedence order
}

// Overrides are flag values; empty strings / zero mean "not set".
type Overrides struct {
	Planner, Reviewer string
	PlansDir, Project string
	Lang              string
	MaxCalls          int
	MaxTime           time.Duration
}

// fileConfig mirrors the TOML surface. Pointers distinguish "unset" from zero values.
type fileConfig struct {
	Planner          *string `toml:"planner"`
	Reviewer         *string `toml:"reviewer"`
	ClaudeCommand    *string `toml:"claude_command"`
	CodexCommand     *string `toml:"codex_command"`
	PlansDir         *string `toml:"plans_dir"`
	Project          *string `toml:"project"`
	Lang             *string `toml:"lang"`
	ReviewRounds     *int    `toml:"review_rounds"`
	DetailBatch      *int    `toml:"detail_batch"`
	CallDeadline     *string `toml:"call_deadline"`
	MaxCalls         *int    `toml:"max_calls"`
	MaxTime          *string `toml:"max_time"`
	MaxContextTokens *int    `toml:"max_context_tokens"`
	Jev              *string `toml:"jev"`
	JevAPIKeyEnv     *string `toml:"jev_api_key_env"`
	JevModel         *string `toml:"jev_model"`
}

// Default returns the built-in defaults.
func Default() Config {
	planner, _ := ParseModelSpec("claude/opus:high") // user decision 2026-09-27: cheaper, keeps the Fable weekly quota
	reviewer, _ := ParseModelSpec("codex/gpt-6-astra:high")
	return Config{
		Planner: planner, Reviewer: reviewer,
		ClaudeCommand: "claude", CodexCommand: "codex",
		ReviewRounds: 6, DetailBatch: 1, CallDeadline: 30 * time.Minute,
		MaxContextTokens: 400_000,
		Jev:              JevOff, JevKeyEnv: JevDefaultEnv, JevModel: JevDefault,
	}
}

// GlobalPath returns $XDG_CONFIG_HOME/shogun/config.toml or ~/.config/shogun/config.toml.
func GlobalPath(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "shogun", "config.toml")
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "shogun", "config.toml")
	}
	return ""
}

// WorkspacePath returns <workspace>/.shogun/config.toml.
func WorkspacePath(workspace string) string {
	return filepath.Join(workspace, ".shogun", "config.toml")
}

// Load merges defaults < global file < workspace file < overrides and validates the result.
// Missing files are fine; unknown keys and invalid values are errors.
func Load(workspace string, ov Overrides, getenv func(string) string) (*Loaded, error) {
	l := &Loaded{Config: Default(), Provenance: map[string]Source{}}
	for _, k := range []string{"planner", "reviewer", "claude_command", "codex_command", "plans_dir", "project", "lang",
		"review_rounds", "detail_batch", "call_deadline", "max_calls", "max_time", "max_context_tokens"} {
		l.Provenance[k] = "default"
	}
	for _, p := range []string{GlobalPath(getenv), WorkspacePath(workspace)} {
		if p == "" {
			continue
		}
		fc, err := readFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		l.Files = append(l.Files, p)
		if err := l.apply(fc, Source("file:"+p), filepath.Dir(p), getenv); err != nil {
			return nil, err
		}
	}
	if err := l.applyOverrides(ov, getenv); err != nil {
		return nil, err
	}
	if err := l.Config.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func readFile(path string) (*fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var fc fileConfig
	dec := toml.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return nil, fmt.Errorf("config %s: unknown key(s):\n%s", path, sm.String())
		}
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &fc, nil
}

func (l *Loaded) apply(fc *fileConfig, src Source, baseDir string, getenv func(string) string) error {
	set := func(key string) { l.Provenance[key] = src }
	var err error
	if fc.Planner != nil {
		if l.Config.Planner, err = ParseModelSpec(*fc.Planner); err != nil {
			return fmt.Errorf("%s: planner: %w", src, err)
		}
		set("planner")
	}
	if fc.Reviewer != nil {
		if l.Config.Reviewer, err = ParseModelSpec(*fc.Reviewer); err != nil {
			return fmt.Errorf("%s: reviewer: %w", src, err)
		}
		set("reviewer")
	}
	if fc.ClaudeCommand != nil {
		l.Config.ClaudeCommand = resolveCommand(*fc.ClaudeCommand, baseDir, getenv)
		set("claude_command")
	}
	if fc.CodexCommand != nil {
		l.Config.CodexCommand = resolveCommand(*fc.CodexCommand, baseDir, getenv)
		set("codex_command")
	}
	if fc.PlansDir != nil {
		l.Config.PlansDir = ResolvePath(*fc.PlansDir, baseDir, getenv)
		set("plans_dir")
	}
	if fc.Project != nil {
		l.Config.Project = *fc.Project
		set("project")
	}
	if fc.Lang != nil {
		l.Config.Lang = *fc.Lang
		set("lang")
	}
	if fc.ReviewRounds != nil {
		l.Config.ReviewRounds = *fc.ReviewRounds
		set("review_rounds")
	}
	if fc.DetailBatch != nil {
		l.Config.DetailBatch = *fc.DetailBatch
		set("detail_batch")
	}
	if fc.CallDeadline != nil {
		if l.Config.CallDeadline, err = time.ParseDuration(*fc.CallDeadline); err != nil {
			return fmt.Errorf("%s: call_deadline: %w", src, err)
		}
		set("call_deadline")
	}
	if fc.MaxCalls != nil {
		l.Config.MaxCalls = *fc.MaxCalls
		set("max_calls")
	}
	if fc.MaxTime != nil {
		if l.Config.MaxTime, err = time.ParseDuration(*fc.MaxTime); err != nil {
			return fmt.Errorf("%s: max_time: %w", src, err)
		}
		set("max_time")
	}
	if fc.MaxContextTokens != nil {
		l.Config.MaxContextTokens = *fc.MaxContextTokens
		set("max_context_tokens")
	}
	if fc.Jev != nil {
		l.Config.Jev = *fc.Jev
		set("jev")
	}
	if fc.JevAPIKeyEnv != nil {
		l.Config.JevKeyEnv = *fc.JevAPIKeyEnv
		set("jev_api_key_env")
	}
	if fc.JevModel != nil {
		l.Config.JevModel = *fc.JevModel
		set("jev_model")
	}
	return nil
}

func (l *Loaded) applyOverrides(ov Overrides, getenv func(string) string) error {
	var err error
	if ov.Planner != "" {
		if l.Config.Planner, err = ParseModelSpec(ov.Planner); err != nil {
			return fmt.Errorf("--planner: %w", err)
		}
		l.Provenance["planner"] = "flag"
	}
	if ov.Reviewer != "" {
		if l.Config.Reviewer, err = ParseModelSpec(ov.Reviewer); err != nil {
			return fmt.Errorf("--reviewer: %w", err)
		}
		l.Provenance["reviewer"] = "flag"
	}
	if ov.PlansDir != "" {
		l.Config.PlansDir = ResolvePath(ov.PlansDir, "", getenv)
		l.Provenance["plans_dir"] = "flag"
	}
	if ov.Project != "" {
		l.Config.Project = ov.Project
		l.Provenance["project"] = "flag"
	}
	if ov.Lang != "" {
		l.Config.Lang = ov.Lang
		l.Provenance["lang"] = "flag"
	}
	if ov.MaxCalls != 0 {
		l.Config.MaxCalls = ov.MaxCalls
		l.Provenance["max_calls"] = "flag"
	}
	if ov.MaxTime != 0 {
		l.Config.MaxTime = ov.MaxTime
		l.Provenance["max_time"] = "flag"
	}
	return nil
}

// ResolvePath expands a leading "~/" and makes relative paths absolute against baseDir
// (the directory of the config file; cwd when baseDir is empty).
func ResolvePath(p, baseDir string, getenv func(string) string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h := getenv("HOME"); h != "" {
			p = filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		if baseDir == "" {
			if abs, err := filepath.Abs(p); err == nil {
				return filepath.Clean(abs)
			}
			return filepath.Clean(p)
		}
		p = filepath.Join(baseDir, p)
	}
	return filepath.Clean(p)
}

// resolveCommand keeps bare names (looked up via PATH later) and resolves paths.
func resolveCommand(c, baseDir string, getenv func(string) string) string {
	if !strings.ContainsRune(c, os.PathSeparator) && !strings.HasPrefix(c, "~") {
		return c
	}
	return ResolvePath(c, baseDir, getenv)
}

// Validate enforces fixed roles, the reviewer effort floor and sane limits.
func (c Config) Validate() error {
	var errs []string
	if c.Planner.Provider != ProviderClaude {
		errs = append(errs, fmt.Sprintf("planner must use provider %q, got %q (roles are fixed in v1)", ProviderClaude, c.Planner.Provider))
	}
	if c.Reviewer.Provider != ProviderCodex {
		errs = append(errs, fmt.Sprintf("reviewer must use provider %q, got %q (roles are fixed in v1)", ProviderCodex, c.Reviewer.Provider))
	}
	if !EffortAtLeast(c.Reviewer.Effort, ReviewerMinEffort) {
		errs = append(errs, fmt.Sprintf("reviewer effort %q is below the hardcoded minimum %q", c.Reviewer.Effort, ReviewerMinEffort))
	}
	if c.ReviewRounds < 1 {
		errs = append(errs, "review_rounds must be >= 1")
	}
	if c.DetailBatch < 1 {
		errs = append(errs, "detail_batch must be >= 1")
	}
	if c.CallDeadline <= 0 {
		errs = append(errs, "call_deadline must be > 0")
	}
	if c.MaxCalls < 0 || c.MaxTime < 0 {
		errs = append(errs, "max_calls and max_time must be >= 0 (0 = derived)")
	}
	if c.MaxContextTokens <= 0 {
		errs = append(errs, "max_context_tokens must be > 0")
	}
	if c.ClaudeCommand == "" || c.CodexCommand == "" {
		errs = append(errs, "claude_command and codex_command must not be empty")
	}
	if c.Project != "" && !ValidProject(c.Project) {
		errs = append(errs, fmt.Sprintf("project %q must be a single safe path segment", c.Project))
	}
	if c.Jev != JevOff && c.Jev != JevAdvisory {
		errs = append(errs, fmt.Sprintf("jev must be %q or %q, not %q", JevOff, JevAdvisory, c.Jev))
	}
	if !validEnvName(c.JevKeyEnv) {
		errs = append(errs, fmt.Sprintf("jev_api_key_env %q is not an environment variable name", c.JevKeyEnv))
	}
	if c.JevModel == "" {
		errs = append(errs, "jev_model must not be empty")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validEnvName accepts [A-Za-z_][A-Za-z0-9_]*.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// ValidProject accepts one path segment of [A-Za-z0-9._-], not "." or "..".
func ValidProject(p string) bool {
	if p == "" || p == "." || p == ".." || len(p) > 64 {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// snapshot is the on-disk TOML shape of an effective configuration (config.snapshot.toml).
// It uses the same keys as fileConfig, so a snapshot can be re-read with readFile.
type snapshot struct {
	Planner          string `toml:"planner"`
	Reviewer         string `toml:"reviewer"`
	ClaudeCommand    string `toml:"claude_command"`
	CodexCommand     string `toml:"codex_command"`
	PlansDir         string `toml:"plans_dir"`
	Project          string `toml:"project"`
	Lang             string `toml:"lang"`
	ReviewRounds     int    `toml:"review_rounds"`
	DetailBatch      int    `toml:"detail_batch"`
	CallDeadline     string `toml:"call_deadline"`
	MaxCalls         int    `toml:"max_calls"`
	MaxTime          string `toml:"max_time"`
	MaxContextTokens int    `toml:"max_context_tokens"`
	Jev              string `toml:"jev"`
	JevAPIKeyEnv     string `toml:"jev_api_key_env"`
	JevModel         string `toml:"jev_model"`
}

// Snapshot renders the effective configuration as valid TOML with provenance in comments.
// Reading it back with LoadSnapshot yields the same Config.
func (l *Loaded) Snapshot() ([]byte, error) {
	c := l.Config
	body, err := toml.Marshal(snapshot{
		Planner: c.Planner.String(), Reviewer: c.Reviewer.String(),
		ClaudeCommand: c.ClaudeCommand, CodexCommand: c.CodexCommand,
		PlansDir: c.PlansDir, Project: c.Project, Lang: c.Lang,
		ReviewRounds: c.ReviewRounds, DetailBatch: c.DetailBatch,
		CallDeadline: c.CallDeadline.String(), MaxCalls: c.MaxCalls, MaxTime: c.MaxTime.String(),
		MaxContextTokens: c.MaxContextTokens,
		Jev:              c.Jev, JevAPIKeyEnv: c.JevKeyEnv, JevModel: c.JevModel,
	})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# shogun effective configuration snapshot (valid TOML; same keys as config.toml)\n")
	keys := make([]string, 0, len(l.Provenance))
	for k := range l.Provenance {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "# provenance: %s = %s\n", k, l.Provenance[k])
	}
	for _, f := range l.Files {
		fmt.Fprintf(&b, "# read: %s\n", f)
	}
	b.WriteString("\n")
	b.Write(body)
	return []byte(b.String()), nil
}

// LoadSnapshot reads a config.snapshot.toml written by Snapshot and validates it.
// Paths are taken verbatim (they were already absolute when snapshotted).
func LoadSnapshot(path string) (*Config, error) {
	fc, err := readFile(path)
	if err != nil {
		return nil, err
	}
	l := &Loaded{Config: Default(), Provenance: map[string]Source{}}
	if err := l.apply(fc, Source("snapshot:"+path), filepath.Dir(path), func(string) string { return "" }); err != nil {
		return nil, err
	}
	if err := l.Config.Validate(); err != nil {
		return nil, err
	}
	return &l.Config, nil
}

// Describe prints every effective value with its source, sorted by key (human-readable, not TOML).
func (l *Loaded) Describe(w io.Writer) {
	c := l.Config
	rows := map[string]string{
		"planner": c.Planner.String(), "reviewer": c.Reviewer.String(),
		"claude_command": c.ClaudeCommand, "codex_command": c.CodexCommand,
		"plans_dir": c.PlansDir, "project": c.Project, "lang": c.Lang,
		"review_rounds": fmt.Sprint(c.ReviewRounds), "detail_batch": fmt.Sprint(c.DetailBatch),
		"call_deadline": c.CallDeadline.String(), "max_calls": fmt.Sprint(c.MaxCalls), "max_time": c.MaxTime.String(),
		"max_context_tokens": fmt.Sprint(c.MaxContextTokens),
		"jev":                c.Jev, "jev_api_key_env": c.JevKeyEnv, "jev_model": c.JevModel,
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := rows[k]
		if v == "" {
			v = `""`
		}
		fmt.Fprintf(w, "%-20s = %-40s # %s\n", k, v, l.Provenance[k])
	}
	if len(l.Files) == 0 {
		fmt.Fprintln(w, "# no config files found; defaults only")
	}
}
