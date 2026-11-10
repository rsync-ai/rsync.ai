package utils

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A number typed into a connection form is stored as JSON and decodes to a
// float64; each flatten site turns it into a string for the connector. The
// string must parse back as the same integer.
func TestConfigString_JSONNumbersStayPlainDecimal(t *testing.T) {
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(`{"max_batch_rows":1000000,"max_file_mb":32,"big":10000000000000000000000,"ratio":1.5,"port":5432}`), &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"max_batch_rows": "1000000",
		"max_file_mb":    "32",
		"big":            "10000000000000000000000",
		"ratio":          "1.5",
		"port":           "5432",
	}
	for k, w := range want {
		if got := ConfigString(cfg[k]); got != w {
			t.Errorf("ConfigString(%s=%v) = %q, want %q", k, cfg[k], got, w)
		}
	}
	if n, err := strconv.ParseInt(ConfigString(cfg["max_batch_rows"]), 10, 64); err != nil || n != 1000000 {
		t.Errorf("1000000 does not round-trip: %d, %v", n, err)
	}
}

// Everything that is not a float prints as %v did, so no other reader changes.
func TestConfigString_NonFloatsUnchanged(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{"abc", "abc"},
		{true, "true"},
		{nil, "<nil>"},
		{42, "42"},
		{float32(2.5), "2.5"},
		{json.Number("1e+06"), "1e+06"},
	}
	for _, c := range cases {
		if got := ConfigString(c.in); got != c.want {
			t.Errorf("ConfigString(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNoConfigFlattenWithPercentV guards the class: a connection config flattened into
// strings must go through ConfigString, never %v. It scans internal/ for the map
// flatten shape `m[k] = fmt.Sprintf("%v", v)` / `fmt.Sprint(v)`.
func TestNoConfigFlattenWithPercentV(t *testing.T) {
	flatten := regexp.MustCompile(`\[[a-zA-Z_]+\] = fmt\.Sprint(f\("%v", )?\(?[a-zA-Z_.]+\)`)
	// Not connection config: connector terminology display strings.
	allowed := map[string]bool{filepath.Join("registry", "connector_registry.go"): true}
	root := ".."
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if flatten.MatchString(line) {
				t.Errorf("%s:%d flattens a value with %%v; use utils.ConfigString: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files under internal/; the walk root is wrong", scanned)
	}
}
