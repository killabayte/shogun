package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Stream limits (§8): no 64 KiB bufio.Scanner limit, but a bounded event and stream size.
const (
	MaxEventBytes  = 16 << 20
	MaxStreamBytes = 64 << 20
)

// killGrace is how long a process group gets between TERM and KILL.
var killGrace = 5 * time.Second

// proc is one subprocess invocation.
type proc struct {
	bin        string
	args       []string
	env        []string
	dir        string
	stdin      string
	stdoutPath string
	stderrPath string
}

// procResult is what happened to the process; the streams are in the files.
type procResult struct {
	exit     int
	limitErr error // a stream exceeded its limit; the process was killed
	stray    bool  // the process exited but a descendant kept the pipes open and was killed
}

// run starts p with stdin closed after the prompt, captures both streams in parallel into files,
// enforces the stream limits and, on cancel/deadline, sends TERM to the whole process group, then
// KILL after killGrace, then waits. Leftover descendants are always killed.
func run(ctx context.Context, p proc) (procResult, error) {
	outF, err := os.OpenFile(p.stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return procResult{}, err
	}
	defer outF.Close()
	errF, err := os.OpenFile(p.stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return procResult{}, err
	}
	defer errF.Close()

	cmd := exec.Command(p.bin, p.args...)
	cmd.Dir, cmd.Env = p.dir, p.env
	cmd.Stdin = strings.NewReader(p.stdin)
	setProcessGroup(cmd)
	limits := &limitState{}
	limits.kill = func() { signalGroup(cmd, true) } // set before Start: the stream writers may call it at once
	cmd.Stdout = &limitWriter{w: outF, lines: true, st: limits}
	cmd.Stderr = &limitWriter{w: errF, st: limits}
	cmd.WaitDelay = time.Second // bound the wait for pipes held open by a stray descendant

	if err := cmd.Start(); err != nil {
		return procResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		signalGroup(cmd, false)
		select {
		case werr = <-done:
		case <-time.After(killGrace):
			signalGroup(cmd, true)
			werr = <-done
		}
	}
	signalGroup(cmd, true) // descendants that outlived the leader
	res := procResult{exit: -1, limitErr: limits.get()}
	if cmd.ProcessState != nil {
		res.exit = cmd.ProcessState.ExitCode()
	}
	res.stray = errors.Is(werr, exec.ErrWaitDelay)
	return res, nil
}

type limitState struct {
	mu   sync.Mutex
	err  error
	kill func()
}

func (s *limitState) set(err error) {
	s.mu.Lock()
	first := s.err == nil
	if first {
		s.err = err
	}
	kill := s.kill
	s.mu.Unlock()
	if first && kill != nil {
		kill()
	}
}

func (s *limitState) get() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// limitWriter writes through to a file and enforces the stream limit and, for JSONL streams, the
// per-event limit (bytes since the last newline).
type limitWriter struct {
	w     io.Writer
	lines bool
	st    *limitState
	total int64
	cur   int64
}

func (l *limitWriter) Write(b []byte) (int, error) {
	l.total += int64(len(b))
	if l.total > MaxStreamBytes {
		err := fmt.Errorf("stream exceeds %d bytes", MaxStreamBytes)
		l.st.set(err)
		return 0, err
	}
	if l.lines {
		rest := b
		for {
			i := bytes.IndexByte(rest, '\n')
			if i < 0 {
				l.cur += int64(len(rest))
				break
			}
			if l.cur+int64(i) > MaxEventBytes {
				l.cur += int64(i)
				break
			}
			l.cur, rest = 0, rest[i+1:]
		}
		if l.cur > MaxEventBytes {
			err := fmt.Errorf("event exceeds %d bytes", MaxEventBytes)
			l.st.set(err)
			return 0, err
		}
	}
	return l.w.Write(b)
}

// readEvents reads a captured JSONL file. A final line without a newline is returned with
// truncated=true: the stream ended mid-event.
func readEvents(path string) (lines [][]byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return lines, true, nil
			}
			if t := bytes.TrimSpace(line); len(t) > 0 {
				lines = append(lines, t)
			}
		}
		if err == io.EOF {
			return lines, false, nil
		}
		if err != nil {
			return lines, false, err
		}
	}
}

// reStartupConfig matches the CLIs' own argument/option diagnostics, printed before any session
// starts (e.g. after a CLI update drops a flag). These are configuration errors: never retried.
var reStartupConfig = regexp.MustCompile(`(?i)(unknown|unrecognized|unexpected) (option|argument|flag)|invalid value .* for|error: .*(option|argument)|error: --[a-z][a-z-]* is not a valid`)

// startupConfigError reports whether a call that produced no events failed on its arguments.
func startupConfigError(lines [][]byte, exit int, stderr []byte) bool {
	return len(lines) == 0 && exit != 0 && reStartupConfig.Match(stderr)
}

// strippedEnv are removed from the child environment so subscription auth and the requested
// model/effort are what the CLI uses (§8). Prefixes cover model/effort/base-URL overrides.
var (
	strippedEnv      = map[string]bool{"CLAUDECODE": true, "CODEX_API_KEY": true, "MAX_THINKING_TOKENS": true, "RUST_LOG": true}
	strippedPrefixes = []string{"ANTHROPIC_", "OPENAI_", "CLAUDE_CODE_", "TYPESAFE_"}

	stripMu    sync.Mutex
	stripExtra = map[string]bool{} // names added at runtime (StripFromChildren)
)

// StripFromChildren adds variable names that must never reach a model's child process — the
// configured Jev key variable, for one: the models must not be able to call Jev themselves.
func StripFromChildren(names ...string) {
	stripMu.Lock()
	defer stripMu.Unlock()
	for _, n := range names {
		if n != "" {
			stripExtra[n] = true
		}
	}
}

func stripped(k string) bool {
	if strippedEnv[k] || hasAnyPrefix(k, strippedPrefixes) {
		return true
	}
	stripMu.Lock()
	defer stripMu.Unlock()
	return stripExtra[k]
}

// childEnv returns base minus the stripped variables, plus extra ("K=V").
func childEnv(base []string, extra ...string) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if stripped(k) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
