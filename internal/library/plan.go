// Package library reads published plans: frontmatter, the approved-body markers, sidecar receipts,
// and provides list/verify over a plans directory.
package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/killabayte/shogun/internal/inputs"
)

// intLiteral matches a plain decimal integer literal (YAML allows _ separators).
var intLiteral = regexp.MustCompile(`^[-+]?[0-9][0-9_]*$`)

// Markers delimit the approved body. Exactly one pair, each on its own line, begin before end.
const (
	MarkerBegin = "<!-- shogun:plan:begin -->"
	MarkerEnd   = "<!-- shogun:plan:end -->"
)

// MutableKeys are the frontmatter keys an executor may change without invalidating approval.
var MutableKeys = map[string]bool{"status": true, "tags": true, "updated": true}

// ValidStatuses of a plan's execution.
var ValidStatuses = []string{"planned", "in_progress", "blocked", "done", "dropped"}

// ErrInvalidFormat marks structural problems (frontmatter, markers).
var ErrInvalidFormat = errors.New("invalid plan format")

// Doc is a parsed plan file.
type Doc struct {
	Frontmatter map[string]any
	Body        []byte // exact bytes between the marker lines (the approval area)
	Trailer     []byte // everything after the end marker line (execution log)
	BodySHA256  string
}

// Parse splits a plan into frontmatter, approved body and trailer, enforcing the format rules.
func Parse(data []byte) (*Doc, error) {
	fm, rest, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	begins := indexLines(rest, MarkerBegin)
	ends := indexLines(rest, MarkerEnd)
	if len(begins) != 1 || len(ends) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one begin and one end marker, found %d/%d", ErrInvalidFormat, len(begins), len(ends))
	}
	if begins[0].start >= ends[0].start {
		return nil, fmt.Errorf("%w: end marker precedes begin marker", ErrInvalidFormat)
	}
	if strings.TrimSpace(string(rest[:begins[0].start])) != "" {
		return nil, fmt.Errorf("%w: only frontmatter and whitespace are allowed before the begin marker", ErrInvalidFormat)
	}
	if err := validateFrontmatter(fm); err != nil {
		return nil, err
	}
	body := rest[begins[0].end:ends[0].start]
	trailer := rest[ends[0].end:]
	sum := sha256.Sum256(body)
	return &Doc{Frontmatter: fm, Body: body, Trailer: trailer, BodySHA256: hex.EncodeToString(sum[:])}, nil
}

// validateFrontmatter enforces the types of the required keys. Values are checked as decoded by
// YAML: `revision: 1.0` is a float and therefore invalid, `revision: "1"` is a string and invalid.
func validateFrontmatter(fm map[string]any) error {
	for _, k := range []string{"title", "plan_id", "project"} {
		v, ok := fm[k]
		if !ok {
			return fmt.Errorf("%w: frontmatter key %q is required", ErrInvalidFormat, k)
		}
		if str, isStr := v.(string); !isStr || strings.TrimSpace(str) == "" {
			return fmt.Errorf("%w: frontmatter key %q must be a non-empty string", ErrInvalidFormat, k)
		}
	}
	rev, ok := fm["revision"]
	if !ok {
		return fmt.Errorf("%w: frontmatter key \"revision\" is required", ErrInvalidFormat)
	}
	switch r := rev.(type) {
	case int64:
		if r < 1 {
			return fmt.Errorf("%w: revision must be >= 1", ErrInvalidFormat)
		}
	case *big.Int:
		return fmt.Errorf("%w: revision %s is out of range", ErrInvalidFormat, r.String())
	default:
		return fmt.Errorf("%w: revision must be an integer, got %T", ErrInvalidFormat, rev)
	}
	switch c := fm["created"].(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%w: created must not be empty", ErrInvalidFormat)
		}
	case time.Time:
	case nil:
		return fmt.Errorf("%w: frontmatter key \"created\" is required", ErrInvalidFormat)
	default:
		return fmt.Errorf("%w: created must be a date or string, got %T", ErrInvalidFormat, fm["created"])
	}
	status, ok := fm["status"].(string)
	if !ok {
		return fmt.Errorf("%w: status must be a string", ErrInvalidFormat)
	}
	valid := false
	for _, st := range ValidStatuses {
		valid = valid || st == status
	}
	if !valid {
		return fmt.Errorf("%w: status %q is not one of %s", ErrInvalidFormat, status, strings.Join(ValidStatuses, "|"))
	}
	return nil
}

type span struct{ start, end int } // end = index after the line's newline (or len)

// indexLines finds lines whose trimmed content equals marker.
func indexLines(data []byte, marker string) []span {
	var out []span
	off := 0
	for off < len(data) {
		nl := bytes.IndexByte(data[off:], '\n')
		lineEnd := len(data)
		next := len(data)
		if nl >= 0 {
			lineEnd = off + nl
			next = lineEnd + 1
		}
		if strings.TrimSpace(string(data[off:lineEnd])) == marker {
			out = append(out, span{off, next})
		}
		off = next
	}
	return out
}

// splitFrontmatter requires a leading "---\n" block closed by a "---" line; keys must be unique.
func splitFrontmatter(data []byte) (map[string]any, []byte, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, fmt.Errorf("%w: missing YAML frontmatter", ErrInvalidFormat)
	}
	rest := data[4:]
	idx := bytes.Index(rest, []byte("\n---\n"))
	var yamlPart, after []byte
	if idx >= 0 {
		yamlPart, after = rest[:idx+1], rest[idx+5:]
	} else if bytes.HasSuffix(rest, []byte("\n---")) {
		yamlPart, after = rest[:len(rest)-3], nil
	} else {
		return nil, nil, fmt.Errorf("%w: unterminated frontmatter", ErrInvalidFormat)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(yamlPart, &node); err != nil {
		return nil, nil, fmt.Errorf("%w: frontmatter YAML: %v", ErrInvalidFormat, err)
	}
	if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%w: frontmatter must be a mapping", ErrInvalidFormat)
	}
	seen := map[string]bool{}
	m := node.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i].Value
		if seen[k] {
			return nil, nil, fmt.Errorf("%w: duplicate frontmatter key %q", ErrInvalidFormat, k)
		}
		seen[k] = true
	}
	decoded, err := decodeNode(m, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: frontmatter: %v", ErrInvalidFormat, err)
	}
	fm, _ := decoded.(map[string]any)
	return fm, after, nil
}

// decodeNode converts a YAML node into exact Go values without going through interface{} decoding,
// which would silently turn integers beyond uint64 into float64. Values: string, bool, nil, int64,
// *big.Int (when it does not fit int64), float64, time.Time, map[string]any, []any.
func decodeNode(n *yaml.Node, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("frontmatter nesting too deep")
	}
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, errors.New("unresolved alias")
		}
		return decodeNode(n.Alias, depth+1)
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return decodeNode(n.Content[0], depth+1)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, errors.New("mapping keys must be scalars")
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("duplicate key %q", k.Value)
			}
			v, err := decodeNode(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := decodeNode(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return decodeScalar(n)
	}
	return nil, fmt.Errorf("unsupported YAML node kind %v", n.Kind)
}

func decodeScalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		// yaml.v3 rejects a literal that is not a null spelling (e.g. `!!null not-null`).
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, err
		}
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, err
		}
		return b, nil
	case "!!int":
		txt := strings.ReplaceAll(n.Value, "_", "")
		bi, ok := new(big.Int).SetString(txt, 0)
		if !ok {
			return nil, fmt.Errorf("invalid !!int value %q", n.Value)
		}
		if bi.IsInt64() {
			return bi.Int64(), nil
		}
		return bi, nil
	case "!!float":
		// yaml.v3 resolves integers that overflow uint64 as !!float; keep them exact instead.
		// An explicit !!float tag is the user's choice and stays a float.
		if n.Style&yaml.TaggedStyle == 0 && intLiteral.MatchString(n.Value) {
			if bi, ok := new(big.Int).SetString(strings.ReplaceAll(n.Value, "_", ""), 10); ok {
				if bi.IsInt64() {
					return bi.Int64(), nil
				}
				return bi, nil
			}
		}
		// yaml.v3's own decoder handles .5, -.5, .inf, .nan and _ separators.
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, err
		}
		return f, nil
	case "!!timestamp":
		var t time.Time
		if err := n.Decode(&t); err != nil {
			return nil, err
		}
		return t, nil
	default: // !!str, !!binary and anything custom stay text
		return n.Value, nil
	}
}

// ImmutableMetadata returns the frontmatter minus the mutable allowlist with every value in canonical
// form (see Canonical). This is what a receipt stores and what Verify compares byte-for-byte.
func ImmutableMetadata(fm map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range fm {
		if !MutableKeys[k] {
			out[k] = Canonical(v)
		}
	}
	return out
}

// Canonical converts an exact decoded value into an unambiguous tagged form. Every value — including
// containers — carries its type, so a user-written map can never collide with a canonical scalar:
//
//	{"t":"null"} · {"t":"bool","v":true} · {"t":"str","v":"…"} · {"t":"int","v":"<decimal, any size>"}
//	{"t":"float","v":"<shortest repr>"} · {"t":"time","v":"<RFC3339Nano UTC>"}
//	{"t":"map","v":{k: canonical…}} · {"t":"list","v":[canonical…]}
//
// Canonical is applied to freshly decoded frontmatter only; a receipt already holds canonical values
// and is compared as stored, never re-encoded.
func Canonical(v any) any {
	switch t := v.(type) {
	case nil:
		return map[string]any{"t": "null"}
	case bool:
		return map[string]any{"t": "bool", "v": t}
	case string:
		return map[string]any{"t": "str", "v": t}
	case int:
		return map[string]any{"t": "int", "v": strconv.FormatInt(int64(t), 10)}
	case int64:
		return map[string]any{"t": "int", "v": strconv.FormatInt(t, 10)}
	case uint64:
		return map[string]any{"t": "int", "v": strconv.FormatUint(t, 10)}
	case *big.Int:
		return map[string]any{"t": "int", "v": t.String()}
	case float64:
		return map[string]any{"t": "float", "v": strconv.FormatFloat(t, 'g', -1, 64)}
	case float32:
		return map[string]any{"t": "float", "v": strconv.FormatFloat(float64(t), 'g', -1, 32)}
	case json.Number:
		lit := t.String()
		if strings.ContainsAny(lit, ".eE") {
			if f, err := strconv.ParseFloat(lit, 64); err == nil {
				return map[string]any{"t": "float", "v": strconv.FormatFloat(f, 'g', -1, 64)}
			}
			return map[string]any{"t": "float", "v": lit}
		}
		if bi, ok := new(big.Int).SetString(lit, 10); ok {
			return map[string]any{"t": "int", "v": bi.String()}
		}
		return map[string]any{"t": "str", "v": lit}
	case time.Time:
		return map[string]any{"t": "time", "v": t.UTC().Format(time.RFC3339Nano)}
	case map[string]any:
		inner := make(map[string]any, len(t))
		for k, val := range t {
			inner[k] = Canonical(val)
		}
		return map[string]any{"t": "map", "v": inner}
	case []any:
		inner := make([]any, len(t))
		for i := range t {
			inner[i] = Canonical(t[i])
		}
		return map[string]any{"t": "list", "v": inner}
	default:
		return map[string]any{"t": "str", "v": fmt.Sprint(v)}
	}
}

// CanonicalJSON renders Canonical(v) as JSON; encoding/json sorts map keys, so the output is stable.
func CanonicalJSON(v any) ([]byte, error) { return json.Marshal(Canonical(v)) }

// Receipt is the portable sidecar <stem>.approval.json written at publication (P5).
type Receipt struct {
	SchemaVersion        int               `json:"schema_version"`
	PlanID               string            `json:"plan_id"`
	Revision             int               `json:"revision"`
	BodySHA256           string            `json:"body_sha256"`
	ImmutableMetadata    map[string]any    `json:"immutable_metadata"`
	ManifestDigest       string            `json:"manifest_digest"`
	RequirementsRevision string            `json:"requirements_revision"`
	ReviewID             string            `json:"review_id"`
	CLIVersions          map[string]string `json:"cli_versions"`
	Requested            map[string]string `json:"requested"`
	Reported             map[string]string `json:"reported"`
	ApprovedAt           string            `json:"approved_at"`
}

// ReceiptPath returns <stem>.approval.json next to a plan.
func ReceiptPath(planPath string) string {
	return strings.TrimSuffix(planPath, filepath.Ext(planPath)) + ".approval.json"
}

// ManifestPath returns <stem>.manifest.json next to a plan: the frozen input manifest of the
// approved generation, published beside the receipt (S0). Unlike the receipt it holds local
// paths, so it is installed with private permissions.
func ManifestPath(planPath string) string {
	return strings.TrimSuffix(planPath, filepath.Ext(planPath)) + ".manifest.json"
}

// Integrity is the verify outcome.
type Integrity string

const (
	Valid         Integrity = "valid"          // exit 0
	Changed       Integrity = "changed"        // exit 1
	Unverifiable  Integrity = "unverifiable"   // exit 2: receipt missing/corrupt
	InvalidFormat Integrity = "invalid_format" // exit 2
)

// ExitCode maps an integrity result to the CLI exit code.
func (i Integrity) ExitCode() int {
	switch i {
	case Valid:
		return 0
	case Changed:
		return 1
	default:
		return 2
	}
}

// Verify checks a plan file against its sidecar receipt.
func Verify(planPath string) (Integrity, string) {
	res, note, _ := verifyPair(planPath)
	return res, note
}

// verifyPair is Verify plus the decoded receipt when the pair is valid.
func verifyPair(planPath string) (Integrity, string, *Receipt) {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return InvalidFormat, err.Error(), nil
	}
	doc, err := Parse(data)
	if err != nil {
		return InvalidFormat, err.Error(), nil
	}
	rdata, err := os.ReadFile(ReceiptPath(planPath))
	if err != nil {
		return Unverifiable, "receipt missing: " + ReceiptPath(planPath), nil
	}
	var r Receipt
	dec := json.NewDecoder(bytes.NewReader(rdata))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil || r.SchemaVersion != 1 || r.BodySHA256 == "" || r.ImmutableMetadata == nil {
		return Unverifiable, "receipt corrupt or unsupported", nil
	}
	if pid, _ := doc.Frontmatter["plan_id"].(string); r.PlanID != "" && pid != r.PlanID {
		return Changed, fmt.Sprintf("plan_id %q does not match receipt %q", pid, r.PlanID), nil
	}
	var reasons []string
	if r.BodySHA256 != doc.BodySHA256 {
		reasons = append(reasons, "body changed")
	}
	got, err := json.Marshal(ImmutableMetadata(doc.Frontmatter))
	if err != nil {
		return Unverifiable, "cannot canonicalize frontmatter: " + err.Error(), nil
	}
	want, err := json.Marshal(r.ImmutableMetadata) // stored canonical form, compared as-is
	if err != nil {
		return Unverifiable, "cannot read receipt metadata: " + err.Error(), nil
	}
	if !bytes.Equal(got, want) {
		reasons = append(reasons, "immutable metadata changed")
	}
	if len(reasons) > 0 {
		return Changed, strings.Join(reasons, "; "), nil
	}
	return Valid, "body and immutable metadata match the receipt", &r
}

// VerifyWithManifest is Verify plus the manifest sidecar (S0): <stem>.manifest.json must be
// present, decodable, of a supported version and internally consistent, and its recomputed
// fingerprint must equal the receipt's manifest digest. A valid pair without the sidecar is
// unverifiable here, while plain Verify keeps accepting it; the sidecar pins the planning base,
// not the approval itself.
func VerifyWithManifest(planPath string) (Integrity, string) {
	res, note, r := verifyPair(planPath)
	if res != Valid {
		return res, note
	}
	mp := ManifestPath(planPath)
	data, err := os.ReadFile(mp)
	if err != nil {
		return Unverifiable, "manifest sidecar missing: " + mp
	}
	m, err := inputs.Decode(data)
	if err != nil {
		return Unverifiable, "manifest sidecar corrupt or unsupported: " + err.Error()
	}
	if r.ManifestDigest == "" {
		return Unverifiable, "receipt carries no manifest digest"
	}
	for _, repo := range m.Repos {
		if repo.ComputeFingerprint() != repo.Fingerprint {
			return Changed, "manifest sidecar repository " + repo.ID + " fingerprint is inconsistent with its recorded digests"
		}
	}
	stored := m.Fingerprint
	got := m.ComputeFingerprint()
	if got != r.ManifestDigest {
		return Changed, "manifest sidecar does not match the receipt's manifest digest"
	}
	if stored != got {
		return Changed, "manifest sidecar fingerprint field is inconsistent with its contents"
	}
	return Valid, "body, immutable metadata and manifest sidecar match the receipt"
}

// Entry is one row of `shogun list`.
type Entry struct {
	Path      string
	Title     string
	Status    string
	Project   string
	Created   string
	PlanID    string
	Integrity Integrity
	Note      string
}

// List scans dir recursively for *.md plans (ignoring the *.approval.json and *.manifest.json
// sidecars) and returns sorted entries.
func List(dir string) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".obsidian" || d.Name() == ".shogun" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		e := Entry{Path: p}
		doc, perr := Parse(data)
		if perr != nil {
			if !bytes.HasPrefix(data, []byte("---\n")) || !bytes.Contains(data, []byte("shogun:plan")) {
				return nil // not a shogun plan at all
			}
			e.Integrity, e.Note = InvalidFormat, perr.Error()
			out = append(out, e)
			return nil
		}
		e.Title = str(doc.Frontmatter["title"])
		e.Status = str(doc.Frontmatter["status"])
		e.Project = str(doc.Frontmatter["project"])
		e.Created = str(doc.Frontmatter["created"])
		e.PlanID = str(doc.Frontmatter["plan_id"])
		e.Integrity, e.Note = Verify(p)
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format("2006-01-02")
	default:
		return fmt.Sprint(v)
	}
}
