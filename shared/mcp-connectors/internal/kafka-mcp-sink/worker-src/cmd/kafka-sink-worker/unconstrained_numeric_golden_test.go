package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// unconstrainedNumericGoldenPath locates shared/unconstrained_numeric_golden.json
// from this package directory. The connectors' batch DDL is checked against the
// same file (shared/mcp-connectors/tests/test_unconstrained_numeric_golden.py), so
// batch and CDC cannot pick different types for the same column.
func unconstrainedNumericGoldenPath() string {
	return filepath.Join("..", "..", "..", "..", "..", "..", "..", "shared", "unconstrained_numeric_golden.json")
}

// TestUnconstrainedNumericGolden_CDCDDL: Debezium sends a NUMERIC column declared
// without precision or scale as a VariableScaleDecimal. No fixed-width decimal holds
// every value it can carry, so PostgreSQL gets plain NUMERIC and MySQL gets TEXT;
// DECIMAL(38,10) cut every value past the tenth decimal place.
func TestUnconstrainedNumericGolden_CDCDDL(t *testing.T) {
	raw, err := os.ReadFile(unconstrainedNumericGoldenPath())
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		CDCDDL map[string]string `json:"cdc_ddl"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(golden.CDCDDL) == 0 {
		t.Fatal("golden has no cdc_ddl cases; the check would pass without checking anything")
	}
	for dest, want := range golden.CDCDDL {
		if got := mapConnectFieldToDDLType(dest, vsdField("amount")); got != want {
			t.Errorf("%s: VariableScaleDecimal maps to %q, want %q", dest, got, want)
		}
	}
}

// TestMapConnectFieldToDDLType_FixedDecimalKeepsItsWidth: a column with a declared
// precision and scale still gets exactly that width.
func TestMapConnectFieldToDDLType_FixedDecimalKeepsItsWidth(t *testing.T) {
	field := map[string]interface{}{
		"field": "price",
		"type":  "bytes",
		"name":  "org.apache.kafka.connect.data.Decimal",
		"parameters": map[string]interface{}{
			"scale":                     "4",
			"connect.decimal.precision": "12",
		},
	}
	for dest, want := range map[string]string{"postgresql": "NUMERIC(12,4)", "mysql": "DECIMAL(12,4)"} {
		if got := mapConnectFieldToDDLType(dest, field); got != want {
			t.Errorf("%s: fixed decimal maps to %q, want %q", dest, got, want)
		}
	}
}

// TestDecodeDecimalBase64_NegativeScale: PostgreSQL 15 allows NUMERIC(p,-s), which
// rounds to the left of the point. Debezium sends the unscaled digits with a negative
// scale, so the value is those digits times ten to the -scale; returning the digits
// alone stored 12300 as 123.
func TestDecodeDecimalBase64_NegativeScale(t *testing.T) {
	cases := []struct {
		name  string
		b64   string
		scale int
		want  string
	}{
		{name: "positive", b64: "ew==", scale: -2, want: "12300"},  // 0x7b = 123
		{name: "negative", b64: "hQ==", scale: -2, want: "-12300"}, // 0x85 = -123
		{name: "zero_scale", b64: "ew==", scale: 0, want: "123"},
		{name: "zero_value", b64: "AA==", scale: -3, want: "0"},
	}
	for _, tc := range cases {
		got, err := decodeDecimalBase64(tc.b64, tc.scale)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: decodeDecimalBase64(%q, %d) = %q, want %q", tc.name, tc.b64, tc.scale, got, tc.want)
		}
	}
}
