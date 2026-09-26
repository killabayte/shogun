// Package inputs materializes explicit inputs (files, URLs) into the run directory,
// builds repository manifests with content-sensitive fingerprints and detects drift.
package inputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ManifestVersion is bumped on incompatible manifest changes.
const ManifestVersion = 1

// Limits bound what a single input may cost.
type Limits struct {
	MaxBytes     int64
	HTTPTimeout  time.Duration
	MaxRedirects int
}

// DefaultLimits: 10 MiB per input, 30 s per request, 5 redirects.
func DefaultLimits() Limits {
	return Limits{MaxBytes: 10 << 20, HTTPTimeout: 30 * time.Second, MaxRedirects: 5}
}

// Source is one explicit input as stored in the manifest.
type Source struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // file | url
	Origin      string `json:"origin"`
	StoredPath  string `json:"stored_path,omitempty"` // relative to run dir
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	FinalURL    string `json:"final_url,omitempty"`
	FetchedAt   string `json:"fetched_at,omitempty"`
	Status      string `json:"status"` // ok | error
	Error       string `json:"error,omitempty"`
}

// ErrUnavailable wraps every input failure; callers map it to needs_input.
var ErrUnavailable = errors.New("input unavailable")

// Materializer copies files and fetches URLs.
type Materializer struct {
	Client *http.Client
	Limits Limits
	Now    func() time.Time
}

// NewMaterializer builds one with sane defaults.
func NewMaterializer() *Materializer {
	l := DefaultLimits()
	m := &Materializer{Limits: l, Now: time.Now}
	m.Client = &http.Client{Timeout: l.HTTPTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= l.MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", l.MaxRedirects)
		}
		return nil
	}}
	return m
}

// Materialize processes origins in order. Every failure is recorded in the returned sources
// and the joined error wraps ErrUnavailable; successful inputs are still stored.
func (m *Materializer) Materialize(ctx context.Context, runDir, cwd string, origins []string) ([]Source, error) {
	var out []Source
	var errs []error
	for i, o := range origins {
		src := Source{ID: fmt.Sprintf("in-%d", i+1), Origin: o}
		var err error
		if isURL(o) {
			src.Kind = "url"
			err = m.fetchURL(ctx, runDir, &src, i+1)
		} else {
			src.Kind = "file"
			err = m.copyFile(runDir, cwd, &src, i+1)
		}
		if err != nil {
			src.Status, src.Error = "error", err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", o, err))
		} else {
			src.Status = "ok"
		}
		out = append(out, src)
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(errs...))
	}
	return out, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func storedName(n int, base string) string {
	base = filepath.Base(base)
	if base == "" || base == "." || base == "/" {
		base = "input"
	}
	return filepath.Join("inputs", fmt.Sprintf("%02d-%s", n, base))
}

func (m *Materializer) copyFile(runDir, cwd string, src *Source, n int) error {
	p := src.Origin
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New("is a directory; pass repositories with --repo")
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (%s)", st.Mode().Type())
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, m.Limits.MaxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > m.Limits.MaxBytes {
		return fmt.Errorf("size exceeds limit %d bytes", m.Limits.MaxBytes)
	}
	if err := checkText(data); err != nil {
		return err
	}
	return m.store(runDir, src, n, data, "")
}

func (m *Materializer) fetchURL(ctx context.Context, runDir string, src *Source, n int) error {
	u, err := url.Parse(src.Origin)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Origin, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "shogun/0 (+https://github.com/killabayte/shogun)")
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d: authentication required; export the page and pass it as a file", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, m.Limits.MaxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > m.Limits.MaxBytes {
		return fmt.Errorf("body exceeds limit %d bytes", m.Limits.MaxBytes)
	}
	if err := checkText(data); err != nil {
		return err
	}
	if looksLikeLogin(data) {
		return errors.New("response looks like a login page; export the content and pass it as a file")
	}
	src.ContentType = resp.Header.Get("Content-Type")
	src.FinalURL = resp.Request.URL.String()
	base := filepath.Base(u.Path)
	if base == "" || base == "." || base == "/" {
		base = u.Host
	}
	return m.store(runDir, src, n, data, base)
}

func (m *Materializer) store(runDir string, src *Source, n int, data []byte, base string) error {
	if base == "" {
		base = src.Origin
	}
	rel := storedName(n, base)
	abs := filepath.Join(runDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	src.StoredPath, src.SHA256, src.Size = rel, hex.EncodeToString(sum[:]), int64(len(data))
	src.FetchedAt = m.Now().UTC().Format(time.RFC3339)
	return nil
}

// checkText enforces the v1 input contract: UTF-8 text only. Binary signatures and NUL bytes are
// reported as unsupported formats; any other invalid UTF-8 is rejected as not-text. Bytes are stored
// exactly as received — nothing is transcoded or replaced.
func checkText(data []byte) error {
	for _, sig := range [][]byte{[]byte("%PDF-"), {0x89, 'P', 'N', 'G'}, {0xFF, 0xD8, 0xFF}, {'P', 'K', 0x03, 0x04}, {0x1F, 0x8B}, {'G', 'I', 'F', '8'}} {
		if bytes.HasPrefix(data, sig) {
			return errors.New("binary format (PDF/image/archive) is not supported in v1; pass an exported text version")
		}
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return errors.New("binary content (NUL bytes) is not supported in v1")
	}
	if !utf8.Valid(data) {
		return errors.New("content is not valid UTF-8 text")
	}
	return nil
}

func looksLikeLogin(data []byte) bool {
	head := strings.ToLower(string(data[:min(len(data), 16384)]))
	return strings.Contains(head, "type=\"password\"") && (strings.Contains(head, "log in") || strings.Contains(head, "login") || strings.Contains(head, "sign in"))
}

// Manifest is inputs.json / manifest.json for a run.
type Manifest struct {
	Version     int      `json:"version"`
	CreatedAt   string   `json:"created_at"`
	Workspace   string   `json:"workspace"`
	Repos       []Repo   `json:"repos"`
	Inputs      []Source `json:"inputs"`
	Fingerprint string   `json:"fingerprint"` // digest over repo fingerprints + input hashes
}

// Fingerprint computes the manifest digest from repo fingerprints and successful input hashes.
func (m *Manifest) ComputeFingerprint() string {
	h := sha256.New()
	for _, r := range m.Repos {
		fmt.Fprintf(h, "repo\x00%s\x00%s\n", r.ID, r.Fingerprint)
	}
	for _, s := range m.Inputs {
		fmt.Fprintf(h, "input\x00%s\x00%s\x00%s\n", s.ID, s.Status, s.SHA256)
	}
	m.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return m.Fingerprint
}

// Save writes the manifest as pretty JSON.
func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Load reads a manifest.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest version %d unsupported", m.Version)
	}
	return &m, nil
}

// sortedKeys is a small helper for deterministic hashing.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
