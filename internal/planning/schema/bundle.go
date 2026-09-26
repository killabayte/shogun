package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ModelKinds are the documents a model produces (and therefore must be strict for Codex).
var ModelKinds = []Kind{KindResearch, KindOutline, KindStep, KindReview}

// Bundle returns a self-contained schema for kind: every cross-file $ref is inlined into a local
// "#/$defs/…" so the document can be passed to a CLI (--json-schema / --output-schema) that cannot
// resolve other files. It validates exactly what Validate(kind) validates.
func Bundle(k Kind) ([]byte, error) {
	root, err := load(string(k) + ".schema.json")
	if err != nil {
		return nil, err
	}
	b := &bundler{defs: map[string]any{}}
	out, err := b.rewrite(root, string(k)+".schema.json")
	if err != nil {
		return nil, err
	}
	doc := out.(map[string]any)
	delete(doc, "$id")
	if len(b.defs) > 0 {
		doc["$defs"] = b.defs
	}
	return json.Marshal(doc)
}

type bundler struct{ defs map[string]any }

func load(name string) (map[string]any, error) {
	data, err := files.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// rewrite copies n, replacing each $ref (relative to file) with a ref into the bundle's $defs.
func (b *bundler) rewrite(n any, file string) (any, error) {
	switch v := n.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, val := range v {
			if key == "$defs" && file != "" {
				continue // pulled in on demand through refs
			}
			if key == "$ref" {
				s, _ := val.(string)
				local, err := b.ref(s, file)
				if err != nil {
					return nil, err
				}
				out[key] = local
				continue
			}
			r, err := b.rewrite(val, file)
			if err != nil {
				return nil, err
			}
			out[key] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			r, err := b.rewrite(val, file)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return n, nil
}

// ref resolves "file#/json/pointer" (file empty = current), copies the target into $defs under a
// name derived from file and pointer, and returns the local ref.
func (b *bundler) ref(ref, file string) (string, error) {
	target, ptr, ok := strings.Cut(ref, "#")
	if !ok || !strings.HasPrefix(ptr, "/") {
		return "", fmt.Errorf("%s: unsupported $ref %q", file, ref)
	}
	if target == "" {
		target = file
	}
	name := strings.TrimSuffix(target, ".schema.json") + strings.ReplaceAll(ptr, "/", ".")
	name = strings.ReplaceAll(strings.ReplaceAll(name, ".$defs.", "."), ".properties.", ".")
	local := "#/$defs/" + name
	if _, done := b.defs[name]; done {
		return local, nil
	}
	b.defs[name] = true // placeholder breaks recursion
	doc, err := load(target)
	if err != nil {
		return "", err
	}
	var node any = doc
	for _, part := range strings.Split(ptr[1:], "/") {
		m, ok := node.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s: $ref %q does not resolve", file, ref)
		}
		if node, ok = m[part]; !ok {
			return "", fmt.Errorf("%s: $ref %q does not resolve", file, ref)
		}
	}
	r, err := b.rewrite(node, target)
	if err != nil {
		return "", err
	}
	b.defs[name] = r
	return local, nil
}
