package library

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const samplePlan = "---\ntitle: Rate limiting\nplan_id: 20260923-143000-rate-limit-a7c9\nrevision: 1\ncreated: 2026-09-23\nupdated: 2026-09-23\nstatus: planned\nproject: portals\nrepos: [api-gateway]\ntags: [shogun, plan]\n---\n\n<!-- shogun:plan:begin -->\n# Plan\n\nbody line\n<!-- shogun:plan:end -->\n\n## Execution log\n| S-001 | todo |\n"

func TestParseSplitsBodyAndTrailer(t *testing.T) {
	doc, err := Parse([]byte(samplePlan))
	if err != nil {
		t.Fatal(err)
	}
	if string(doc.Body) != "# Plan\n\nbody line\n" {
		t.Fatalf("body = %q", doc.Body)
	}
	if !strings.HasPrefix(string(doc.Trailer), "\n## Execution log") {
		t.Fatalf("trailer = %q", doc.Trailer)
	}
	if doc.Frontmatter["title"] != "Rate limiting" || doc.Frontmatter["revision"] != int64(1) {
		t.Fatalf("frontmatter = %v", doc.Frontmatter)
	}
	im := ImmutableMetadata(doc.Frontmatter)
	for _, k := range []string{"status", "tags", "updated"} {
		if _, ok := im[k]; ok {
			t.Errorf("%s must be mutable", k)
		}
	}
}

func TestParseRejectsBadFormats(t *testing.T) {
	cases := map[string]string{
		"revision float":     strings.Replace(samplePlan, "revision: 1\n", "revision: 1.0\n", 1),
		"revision string":    strings.Replace(samplePlan, "revision: 1\n", "revision: \"1\"\n", 1),
		"bad status":         strings.Replace(samplePlan, "status: planned", "status: finished", 1),
		"missing plan_id":    strings.Replace(samplePlan, "plan_id: 20260923-143000-rate-limit-a7c9\n", "", 1),
		"no frontmatter":     "# Plan\n<!-- shogun:plan:begin -->\nx\n<!-- shogun:plan:end -->\n",
		"duplicate key":      "---\ntitle: a\ntitle: b\n---\n<!-- shogun:plan:begin -->\nx\n<!-- shogun:plan:end -->\n",
		"two begin markers":  "---\ntitle: a\n---\n<!-- shogun:plan:begin -->\n```\n<!-- shogun:plan:begin -->\n```\n<!-- shogun:plan:end -->\n",
		"reversed markers":   "---\ntitle: a\n---\n<!-- shogun:plan:end -->\nx\n<!-- shogun:plan:begin -->\n",
		"text before begin":  "---\ntitle: a\n---\nintro text\n<!-- shogun:plan:begin -->\nx\n<!-- shogun:plan:end -->\n",
		"missing end marker": "---\ntitle: a\n---\n<!-- shogun:plan:begin -->\nx\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); !errors.Is(err, ErrInvalidFormat) {
			t.Errorf("%s: expected ErrInvalidFormat, got %v", name, err)
		}
	}
}

func writeReceipt(t *testing.T, planPath string, doc *Doc) {
	t.Helper()
	r := Receipt{SchemaVersion: 1, PlanID: "20260923-143000-rate-limit-a7c9", Revision: 1, BodySHA256: doc.BodySHA256,
		ImmutableMetadata: ImmutableMetadata(doc.Frontmatter)}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(ReceiptPath(planPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyValidChangedUnverifiable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "portals", "plan.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(samplePlan), 0o644)
	if got, _ := Verify(p); got != Unverifiable || got.ExitCode() != 2 {
		t.Fatalf("no receipt → unverifiable, got %s", got)
	}
	doc, _ := Parse([]byte(samplePlan))
	writeReceipt(t, p, doc)
	if got, note := Verify(p); got != Valid || got.ExitCode() != 0 {
		t.Fatalf("expected valid, got %s (%s)", got, note)
	}
	// mutable edits keep it valid
	mut := strings.Replace(samplePlan, "status: planned", "status: done", 1) + "| S-001 | done |\n"
	os.WriteFile(p, []byte(mut), 0o644)
	if got, note := Verify(p); got != Valid {
		t.Fatalf("status/log edits must stay valid, got %s (%s)", got, note)
	}
	// body edit → changed
	os.WriteFile(p, []byte(strings.Replace(samplePlan, "body line", "body line edited", 1)), 0o644)
	if got, _ := Verify(p); got != Changed || got.ExitCode() != 1 {
		t.Fatalf("body edit → changed, got %s", got)
	}
	// immutable metadata edit → changed
	os.WriteFile(p, []byte(strings.Replace(samplePlan, "project: portals", "project: other", 1)), 0o644)
	if got, note := Verify(p); got != Changed || !strings.Contains(note, "immutable metadata") {
		t.Fatalf("metadata edit → changed, got %s (%s)", got, note)
	}
	// reordered keys / quoting keep values → valid
	reordered := strings.Replace(samplePlan, "title: Rate limiting\nplan_id:", "plan_id:", 1)
	reordered = strings.Replace(reordered, "project: portals\n", "project: \"portals\"\ntitle: 'Rate limiting'\n", 1)
	os.WriteFile(p, []byte(reordered), 0o644)
	if got, note := Verify(p); got != Valid {
		t.Fatalf("reordering keys must stay valid, got %s (%s)", got, note)
	}
	// corrupt receipt → unverifiable
	os.WriteFile(ReceiptPath(p), []byte("{"), 0o644)
	if got, _ := Verify(p); got != Unverifiable {
		t.Fatalf("corrupt receipt → unverifiable, got %s", got)
	}
}

func TestCanonicalKeepsExactNumbersAndTypes(t *testing.T) {
	distinct := func(name string, a, b any) {
		t.Helper()
		ja, _ := CanonicalJSON(a)
		jb, _ := CanonicalJSON(b)
		if string(ja) == string(jb) {
			t.Errorf("%s: %s must differ from %s", name, ja, jb)
		}
	}
	distinct("2^53 neighbours", int64(9007199254740992), int64(9007199254740993))
	big1, _ := new(big.Int).SetString("18446744073709551616", 10)
	big2, _ := new(big.Int).SetString("18446744073709551617", 10)
	distinct("beyond uint64", big1, big2)
	distinct("int vs float", int64(1), 1.0)
	distinct("int vs string", int64(1), "1")
	distinct("scalar vs user map that mimics a tag", int64(1), map[string]any{"__int__": "1"})
	distinct("scalar vs user map that mimics canonical form", int64(1), map[string]any{"t": "int", "v": "1"})
	distinct("null vs empty string", nil, "")
	// receipt round-trip: canonical JSON decodes back to the same JSON without re-encoding
	src, _ := CanonicalJSON(map[string]any{"n": big1, "when": time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), "tags": []any{"a", int64(2)}})
	var back any
	dec := json.NewDecoder(strings.NewReader(string(src)))
	dec.UseNumber()
	dec.Decode(&back)
	again, _ := json.Marshal(back)
	if string(again) != string(src) {
		t.Fatalf("stored canonical form must re-marshal identically: %s vs %s", again, src)
	}
}

func TestParseDecodesExactYAMLValues(t *testing.T) {
	src := strings.Replace(samplePlan, "revision: 1\n", "revision: 1\nbig: 18446744073709551616\nneg: -5\nhex: 0x1F\nratio: 0.5\nflag: true\nnothing: null\nnested: {a: [1, two]}\n", 1)
	doc, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	fm := doc.Frontmatter
	if b, ok := fm["big"].(*big.Int); !ok || b.String() != "18446744073709551616" {
		t.Errorf("big int not exact: %T %v", fm["big"], fm["big"])
	}
	if fm["neg"] != int64(-5) || fm["hex"] != int64(31) || fm["ratio"] != 0.5 || fm["flag"] != true || fm["nothing"] != nil {
		t.Errorf("scalars decoded wrong: %v %v %v %v %v", fm["neg"], fm["hex"], fm["ratio"], fm["flag"], fm["nothing"])
	}
	if _, ok := fm["created"].(time.Time); !ok {
		t.Errorf("created should decode as timestamp, got %T", fm["created"])
	}
	nested, _ := fm["nested"].(map[string]any)
	list, _ := nested["a"].([]any)
	if len(list) != 2 || list[0] != int64(1) || list[1] != "two" {
		t.Errorf("nested decode wrong: %#v", fm["nested"])
	}
}

func TestListScansPlansAndSkipsForeignMarkdown(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "portals", "a.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(samplePlan), 0o644)
	doc, _ := Parse([]byte(samplePlan))
	writeReceipt(t, p, doc)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# not a plan\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\ntitle: x\n---\n<!-- shogun:plan:begin -->\nno end\n"), 0o644)
	entries, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (plan + broken), got %d: %+v", len(entries), entries)
	}
	var okE, brokenE *Entry
	for i := range entries {
		if entries[i].Integrity == Valid {
			okE = &entries[i]
		} else {
			brokenE = &entries[i]
		}
	}
	if okE == nil || okE.Title != "Rate limiting" || okE.Status != "planned" || okE.Project != "portals" {
		t.Fatalf("plan entry wrong: %+v", okE)
	}
	if brokenE == nil || brokenE.Integrity != InvalidFormat {
		t.Fatalf("broken plan must be listed as invalid_format: %+v", brokenE)
	}
}

// S0: the manifest sidecar sits next to the receipt and never shows up as a plan.
func TestManifestPathAndListIgnoresSidecars(t *testing.T) {
	if got := ManifestPath("/x/docs/plans/feature.md"); got != "/x/docs/plans/feature.manifest.json" {
		t.Fatalf("ManifestPath: %s", got)
	}
	dir := t.TempDir()
	plan := "---\ntitle: Demo\nplan_id: p1\nrevision: 1\ncreated: 2026-09-23\nstatus: planned\nproject: demo\n---\n<!-- shogun:plan:begin -->\nbody\n<!-- shogun:plan:end -->\n"
	p := filepath.Join(dir, "p1.md")
	os.WriteFile(p, []byte(plan), 0o644)
	doc, err := Parse([]byte(plan))
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := json.Marshal(Receipt{SchemaVersion: 1, PlanID: "p1", Revision: 1, BodySHA256: doc.BodySHA256, ImmutableMetadata: ImmutableMetadata(doc.Frontmatter)})
	os.WriteFile(ReceiptPath(p), rc, 0o644)
	os.WriteFile(ManifestPath(p), []byte("{\"version\": 1}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "stray.manifest.json"), []byte("{}\n"), 0o600)
	entries, err := List(dir)
	if err != nil || len(entries) != 1 || entries[0].Path != p || entries[0].Integrity != Valid {
		t.Fatalf("List: %v %+v", err, entries)
	}
}
