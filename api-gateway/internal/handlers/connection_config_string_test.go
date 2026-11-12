package handlers

import (
	"encoding/json"
	"strconv"
	"testing"
)

// A test-connection config decoded from JSON holds float64 numbers; each must reach
// the orchestrator as a plain decimal an integer parser accepts.
func TestConfigValueString_JSONNumbersStayPlainDecimal(t *testing.T) {
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(`{"max_batch_rows":1000000,"port":5432,"ratio":1.5,"ssl":true,"host":"db"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"max_batch_rows": "1000000", "port": "5432", "ratio": "1.5", "ssl": "true", "host": "db"}
	for k, w := range want {
		if got := configValueString(cfg[k]); got != w {
			t.Errorf("%s: got %q, want %q", k, got, w)
		}
	}
	if _, err := strconv.ParseInt(configValueString(cfg["max_batch_rows"]), 10, 64); err != nil {
		t.Errorf("max_batch_rows does not parse as an integer: %v", err)
	}
}
