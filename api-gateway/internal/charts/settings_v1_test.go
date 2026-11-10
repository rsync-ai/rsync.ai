package charts

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

type settingsPair struct {
	Name string          `json:"name"`
	V1   json.RawMessage `json:"v1"`
	V2   json.RawMessage `json:"v2"`
}

func readSettingsPairs(t *testing.T) []settingsPair {
	t.Helper()
	raw, err := os.ReadFile("../../../shared/chart_settings_v1_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []settingsPair `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 10 {
		t.Fatalf("%d cases; every v1 field needs one", len(f.Cases))
	}
	return f.Cases
}

// sameJSON compares two JSON values by their content, not their key order or spacing.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y interface{}
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

// A chart saved as version 1 decodes to the version 2 the migration writes, and that
// version 2 decodes to itself: the gateway reads both shapes for one release.
func TestDecodeUpgradesVersion1(t *testing.T) {
	for _, c := range readSettingsPairs(t) {
		t.Run(c.Name, func(t *testing.T) {
			for _, in := range []json.RawMessage{c.V1, c.V2} {
				s, err := Decode(in)
				if err != nil {
					t.Fatalf("Decode(%s) = %v", in, err)
				}
				got, _ := json.Marshal(s)
				if !sameJSON(t, got, c.V2) {
					t.Fatalf("Decode(%s) = %s, want %s", in, got, c.V2)
				}
			}
		})
	}
}

// Version 1 keeps its own unknown-key check: a v2 key in a v1 body, or a v1 key in a v2
// body, is refused rather than dropped.
func TestDecodeKeepsEachVersionsKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"grain in a v1 axis":     `{"type":"bar","x":{"column":"day","grain":"month"},"measure":{"aggregate":"count"}}`,
		"roles in a v1 body":     `{"type":"bar","roles":{"x":[{"column":"day"}]},"measure":{"aggregate":"count"}}`,
		"x in a v2 body":         `{"version":2,"type":"bar","x":{"column":"day"},"roles":{"measure":[{"aggregate":"count"}]}}`,
		"compare column in v2":   `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"d"}]},"compare":{"column":"d","days":7}}`,
		"colors in a v1 measure": `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count","colors":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(raw))
			if ce, ok := AsError(err); !ok || ce.Rule != 1 {
				t.Fatalf("Decode(%s) = %v, want refused by rule 1", raw, err)
			}
		})
	}
}

// A reader hands on version 2 whichever version is stored, and leaves settings that no
// longer decode as they are.
func TestAsVersion2(t *testing.T) {
	for _, c := range readSettingsPairs(t) {
		for _, in := range []json.RawMessage{c.V1, c.V2} {
			if got := AsVersion2(in); !sameJSON(t, got, c.V2) {
				t.Errorf("%s: AsVersion2(%s) = %s, want %s", c.Name, in, got, c.V2)
			}
		}
	}
	bad := json.RawMessage(`{"type":"pie","x":{"column":"day"}}`)
	if got := AsVersion2(bad); string(got) != string(bad) {
		t.Errorf("AsVersion2(%s) = %s, want it as it is", bad, got)
	}
}
