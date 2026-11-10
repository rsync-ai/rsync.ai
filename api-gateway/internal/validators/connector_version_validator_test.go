package validators

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The validator found a connector only by a metadata.json in the connector's own
// folder. Every connector in the catalog (shipped or generated) keeps it under
// versions/<v>/ next to latest.json, with no root copy, so every connector was
// "not found": a version change on a connection answered 404 whatever the version.
// The class is "a lookup that knows one layout", so each layout gets a case,
// including the legacy root-metadata one the old code did handle.
func TestTheVersionValidatorFindsEveryConnectorLayout(t *testing.T) {
	base := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	versioned := func(rel, id string) {
		t.Helper()
		write(rel+"/latest.json", `{"current_version":"v1.0.1","all_versions":["v1.0.0","v1.0.1"],"breaking_changes":{"v1.0.1":["renamed a tool"]}}`)
		write(rel+"/versions/v1.0.0/metadata.json", `{"id":"`+id+`"}`)
		write(rel+"/versions/v1.0.1/metadata.json", `{"id":"`+id+`","aliases":["`+id+`-alias"]}`)
	}
	versioned("public/acme-crm", "acme-crm")                  // generated connector (flat)
	versioned("public/database/acme-db", "acme-db")           // category layout
	versioned("internal/plumbing-thing", "plumbing-thing")    // internal root
	versioned("public/storage/odd-folder", "blob-store")      // folder name differs from the id
	write("public/old-api/metadata.json", `{"id":"old-api"}`) // legacy root metadata
	write("public/old-api/versions/v1.0.0/metadata.json", `{"id":"old-api"}`)

	v := NewConnectorVersionValidator(base)
	for _, id := range []string{"acme-crm", "acme_crm", "acme-db", "plumbing-thing", "blob-store", "blob-store-alias", "old-api"} {
		if err := v.ValidateVersionExists(id, "v1.0.0"); err != nil {
			t.Errorf("ValidateVersionExists(%q, v1.0.0) = %v, want nil", id, err)
		}
	}
	if err := v.ValidateVersionExists("acme-crm", "v9.9.9"); err == nil {
		t.Error("a version that is not on disk was accepted")
	}
	if err := v.ValidateVersionExists("not-there", "v1.0.0"); err == nil {
		t.Error("a connector that is not on disk was accepted")
	}

	got, err := v.GetAvailableVersions("acme-crm")
	if err != nil || !reflect.DeepEqual(got, []string{"latest", "v1.0.0", "v1.0.1"}) {
		t.Errorf("GetAvailableVersions = %v, %v", got, err)
	}
	if has, changes, err := v.CheckBreakingChanges("acme-db", "v1.0.0", "v1.0.1"); err != nil || !has || len(changes) != 1 {
		t.Errorf("CheckBreakingChanges = %v %v %v, want the latest.json entry", has, changes, err)
	}
}

// The repository's own catalog, so a layout change there that the validator cannot
// read fails here rather than as a 404 on a connection edit.
func TestTheVersionValidatorFindsTheShippedCatalog(t *testing.T) {
	catalog := filepath.Join("..", "..", "..", "shared", "mcp-connectors")
	if _, err := os.Stat(catalog); err != nil {
		t.Skipf("no catalog at %s", catalog)
	}
	v := NewConnectorVersionValidator(catalog)
	for _, id := range []string{"stripe", "postgresql", "bigquery", "aws-s3", "kafka-mcp-sink"} {
		if err := v.ValidateVersionExists(id, "v1.0.0"); err != nil {
			t.Errorf("ValidateVersionExists(%q, v1.0.0) = %v", id, err)
		}
	}
}
