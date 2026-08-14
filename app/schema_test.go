package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

/*
The loader validates meaning: that phases cover every week, that a guide a
session names exists. What it cannot see is a field that is not a field.
encoding/json drops an unknown key silently, so "lable" is not an error — it is
a session with no label that validates, because the label it should have had
went in the bin on the way past.

So this walks each data file against the struct it is loaded into and reports
every key with nowhere to go. It also writes the JSON Schema files an editor
uses for autocomplete, from the same reflection, because a schema maintained
separately from the struct is one more pair of things that can disagree — which
is the failure this whole corner of the repo exists to prevent.

Both live in a _test.go file on purpose: SRC_HASH excludes tests, so none of it
reaches the image and no schema file can ever become something the running
container needs in order to start.

	make check-schema   # verify
	make schema         # rewrite app/schema/*.json after changing a struct
*/

var updateSchema = flag.Bool("update-schema", false, "rewrite app/schema/*.json from the structs")

// dataShapes maps a file glob to the type it is unmarshalled into.
func dataShapes() []struct {
	glob string
	typ  reflect.Type
	name string
} {
	return []struct {
		glob string
		typ  reflect.Type
		name string
	}{
		{"athlete.json", reflect.TypeOf(Athlete{}), "athlete"},
		{"blocks/*.json", reflect.TypeOf(Block{}), "block"},
		{"library/index.json", reflect.TypeOf(Index{}), "index"},
		{"library/*.json", reflect.TypeOf(GuideFile{}), "library"},
	}
}

// TestDataFilesHaveNoUnknownFields is the check the loader structurally cannot
// do: a misspelt key is not a load error, it is a silently missing value.
func TestDataFilesHaveNoUnknownFields(t *testing.T) {
	for _, dir := range []string{"data", "defaults"} {
		for _, shape := range dataShapes() {
			paths, _ := filepath.Glob(filepath.Join(dir, shape.glob))
			for _, p := range paths {
				// index.json is an Index, not a GuideFile; it is matched by both.
				if shape.name == "library" && filepath.Base(p) == "index.json" {
					continue
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("%s: %v", p, err)
				}
				var any any
				if err := json.Unmarshal(raw, &any); err != nil {
					t.Fatalf("%s: %v", p, err)
				}
				for _, bad := range unknownKeys(any, shape.typ, "") {
					t.Errorf("%s: %s — no such field in %s; encoding/json would drop it silently",
						p, bad, shape.typ.Name())
				}
			}
		}
	}
}

// unknownKeys walks a decoded document beside the type it will be loaded into
// and returns the paths of keys the type has no field for.
func unknownKeys(v any, t reflect.Type, path string) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch node := v.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := jsonFields(t)
			var out []string
			for _, k := range sortedKeys(node) {
				f, ok := fields[k]
				if !ok {
					out = append(out, join(path, k))
					continue
				}
				out = append(out, unknownKeys(node[k], f.Type, join(path, k))...)
			}
			return out
		case reflect.Map:
			// An open map: the keys are the author's, the values are typed.
			var out []string
			for _, k := range sortedKeys(node) {
				out = append(out, unknownKeys(node[k], t.Elem(), join(path, k))...)
			}
			return out
		}
		return nil // a custom unmarshaller or interface{}; not ours to police
	case []any:
		if t.Kind() != reflect.Slice {
			return nil
		}
		var out []string
		for i, item := range node {
			out = append(out, unknownKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	}
	return nil
}

// jsonFields is a struct's json tag names, following embedded structs.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			if f.PkgPath != "" {
				continue // unexported and untagged: not part of the document
			}
			name = f.Name
		}
		out[name] = f
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

/* ── the editor schemas ────────────────────────────────────────────────── */

// TestSchemasMatchTheStructs keeps app/schema/*.json in step with the types.
// Run with -update-schema to rewrite them.
func TestSchemasMatchTheStructs(t *testing.T) {
	for _, shape := range dataShapes() {
		want, err := json.MarshalIndent(schemaFor(shape.typ, shape.name), "", "  ")
		if err != nil {
			t.Fatalf("%s: %v", shape.name, err)
		}
		want = append(want, '\n')

		path := filepath.Join("schema", shape.name+".schema.json")
		if *updateSchema {
			if err := os.MkdirAll("schema", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", path)
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v — run 'make schema'", path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s is out of step with the Go types — run 'make schema'", path)
		}
	}
}

// schemaFor reflects a type into a JSON Schema. It is deliberately shallow on
// the quantity types: "7 mi" is a string to a schema, and the unit rules are
// enforced by the parser rather than restated here in a second dialect.
func schemaFor(t reflect.Type, title string) map[string]any {
	s := jsonSchema(t)
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["title"] = title
	return s
}

func jsonSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// Types with their own UnmarshalJSON take a string carrying its unit.
	switch t.Name() {
	case "Distance":
		return map[string]any{"type": "string", "description": `a distance with its unit, e.g. "7 mi", "12 km", "400 m"`}
	case "Pace":
		return map[string]any{"type": "string", "description": `a pace with its unit, e.g. "9:45/mi", "4:30/km"`}
	case "Weight":
		return map[string]any{"type": "string", "description": `a weight with its unit, e.g. "25 lb", "12 kg"`}
	case "WeekRange":
		return map[string]any{"type": "string", "description": `a span of weeks: "1-4", "7", or "14+"`}
	case "Units":
		return map[string]any{"type": "string", "enum": []string{"imperial", "metric"}}
	case "HRBound":
		return map[string]any{"type": []string{"integer", "string"}, "description": `a bpm number or a template like "{{.Athlete.HR.easyLo}}"`}
	case "PowerBound":
		return map[string]any{"type": []string{"integer", "string"}, "description": `a watts number or a template like "{{pct 62 .Athlete.Power.ftp}}"`}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		fields := jsonFields(t)
		for name, f := range fields {
			props[name] = jsonSchema(f.Type)
			if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				required = append(required, name)
			}
		}
		sort.Strings(required)
		out := map[string]any{
			"type": "object", "properties": props,
			"additionalProperties": false,
		}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	case reflect.Slice:
		return map[string]any{"type": "array", "items": jsonSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": jsonSchema(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{}
}
