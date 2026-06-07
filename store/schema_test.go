package store

import (
	"context"
	"flag"
	"os"
	"testing"
)

// updateSchema, set via `go test ./store -update-schema`, rewrites the
// checked-in snapshot from the live migration chain instead of comparing
// against it. Running it is the sanctioned way to refresh schema.sql after
// adding a migration.
var updateSchema = flag.Bool("update-schema", false, "rewrite store/schema.sql from the current migration chain")

const schemaSnapshotPath = "schema.sql"

// TestSchemaSnapshot guards store/schema.sql against drift: the checked-in
// snapshot must equal the DDL produced by migrating a fresh database to
// SchemaVersion. A migration that changes the shape without regenerating
// the snapshot fails here, which keeps the human/agent-readable schema
// honest without anyone having to remember to refresh it.
func TestSchemaSnapshot(t *testing.T) {
	want, err := canonicalSchemaSQL(context.Background())
	if err != nil {
		t.Fatalf("generate canonical schema: %v", err)
	}

	if *updateSchema {
		if err := os.WriteFile(schemaSnapshotPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", schemaSnapshotPath, err)
		}
		t.Logf("wrote %s", schemaSnapshotPath)
		return
	}

	got, err := os.ReadFile(schemaSnapshotPath)
	if err != nil {
		t.Fatalf("read %s (run `go test ./store -update-schema` to create it): %v", schemaSnapshotPath, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale — run `go test ./store -update-schema` to regenerate it", schemaSnapshotPath)
	}
}
