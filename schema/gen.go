// Package schema holds the stack definition's JSON Schema (stack.v1.json), generated from internal/def's types so
// the two never drift: `go test ./schema -update` rewrites it, and a test fails while the file differs.
package schema

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// V1 is stack.v1.json.
//
//go:embed stack.v1.json
var V1 []byte

// ID is the schema's identifier.
const ID = "https://github.com/runsnip/ops/schema/stack.v1.json"

// Generate builds the schema for a root type. enums maps a dotted path (with [] for list items and * for map
// values) to its allowed values; docs maps a path to its description.
func Generate(root reflect.Type, enums map[string][]string, docs map[string]string) ([]byte, error) {
	s := walk(root, "", enums, docs)
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = ID
	s["title"] = "runsnip stack definition (stack standard v1)"
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func walk(t reflect.Type, path string, enums map[string][]string, docs map[string]string) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]any{}
	switch t.Kind() {
	case reflect.String:
		out["type"] = "string"
	case reflect.Bool:
		out["type"] = "boolean"
	case reflect.Int, reflect.Int64:
		out["type"] = "integer"
	case reflect.Float64:
		out["type"] = "number"
	case reflect.Slice:
		out["type"] = "array"
		out["items"] = walk(t.Elem(), path+"[]", enums, docs)
	case reflect.Map:
		out["type"] = "object"
		if t.Key().Kind() == reflect.Int {
			out["propertyNames"] = map[string]any{"pattern": "^[0-9]+$"}
		}
		out["additionalProperties"] = walk(t.Elem(), path+".*", enums, docs)
	case reflect.Struct:
		out["type"] = "object"
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("yaml")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				continue
			}
			p := strings.TrimPrefix(path+"."+name, ".")
			props[name] = walk(f.Type, p, enums, docs)
			if !strings.Contains(opts, "omitempty") {
				required = append(required, name)
			}
		}
		out["properties"] = props
		out["additionalProperties"] = false
		if len(required) > 0 {
			sort.Strings(required)
			out["required"] = required
		}
	}
	if e, ok := enums[path]; ok {
		out["enum"] = e
	}
	if d, ok := docs[path]; ok {
		out["description"] = d
	}
	return out
}
