// Package specshape checks that a JSON resource has exactly the fields an
// OpenAPI component schema documents. It reads the repo's api/*.openapi.yaml
// files, which follow one layout (two-space indents, schemas under
// components.schemas, `required: [...]` flow lists), so no YAML library is
// needed.
package specshape

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Schema is one component schema's top-level field names.
type Schema struct {
	Required   []string
	Properties []string
}

var propRe = regexp.MustCompile(`^        ([A-Za-z_][A-Za-z0-9_]*):`)

// Load returns components.schemas.<name> of the spec at path.
func Load(t testing.TB, path, name string) Schema {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test helper; path is a repo file
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	inSchemas := false
	for i, l := range lines {
		if l == "  schemas:" {
			inSchemas = true
			continue
		}
		if inSchemas && l == "    "+name+":" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no schema %s", path, name)
	}
	var s Schema
	inProps, inReq := false, false
	var req strings.Builder
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "     ") {
			break // next schema (indent <= 4)
		}
		switch {
		case inReq:
			req.WriteString(l)
			inReq = !strings.Contains(l, "]")
		case strings.HasPrefix(l, "      required: ["):
			req.WriteString(strings.TrimPrefix(l, "      required: "))
			inReq = !strings.Contains(l, "]")
		case l == "      properties:":
			inProps = true
		case strings.HasPrefix(l, "      ") && !strings.HasPrefix(l, "       "):
			inProps = false
		case inProps && propRe.MatchString(l):
			s.Properties = append(s.Properties, propRe.FindStringSubmatch(l)[1])
		}
	}
	for _, f := range strings.Split(strings.Trim(strings.TrimSpace(req.String()), "[]"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			s.Required = append(s.Required, f)
		}
	}
	if len(s.Properties) == 0 {
		t.Fatalf("%s: schema %s has no properties", path, name)
	}
	return s
}

// Keys returns the top-level keys of v marshalled to JSON (an object).
func Keys(t testing.TB, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not an object: %v", b, err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Match fails t unless v's JSON keys are exactly the schema's properties
// and include every required field. Exact matching is deliberate: a field
// the spec does not document must not reach a client (on public pages it
// could leak data).
func Match(t testing.TB, path, name string, v any) {
	t.Helper()
	s := Load(t, path, name)
	got := Keys(t, v)
	for _, k := range got {
		if !slices.Contains(s.Properties, k) {
			t.Errorf("%s: field %q is not documented in %s", name, k, path)
		}
	}
	for _, k := range s.Properties {
		if !slices.Contains(got, k) {
			t.Errorf("%s: documented field %q is missing from the response", name, k)
		}
	}
	for _, k := range s.Required {
		if !slices.Contains(got, k) {
			t.Errorf("%s: required field %q is missing from the response", name, k)
		}
	}
}
