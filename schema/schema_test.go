package schema_test

import (
	"bytes"
	"flag"
	"os"
	"testing"

	"github.com/runsnip/ops/internal/standard"
	"github.com/runsnip/ops/schema"
)

var update = flag.Bool("update", false, "rewrite stack.v1.json")

// The committed schema is the one the types generate.
func TestSchemaIsGeneratedFromTheTypes(t *testing.T) {
	b, err := standard.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("stack.v1.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(b, schema.V1) {
		t.Fatal("schema/stack.v1.json is stale: go test ./schema -update")
	}
}
