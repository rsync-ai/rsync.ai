package utils

import (
	"fmt"
	"strconv"
)

// ConfigString renders one decoded connection-config value as the string a
// connector or the sink worker parses.
//
// A connection's config is stored as JSON, so every number in it decodes to a
// float64, and fmt's %v prints a float64 of 1e21 or more — and any whole number
// from 1e6 up — in exponent form: a typed 1000000 became "1e+06", which
// strconv.ParseInt (Go) and int() (Python) both reject, so the setting was
// silently ignored. A float is printed in plain decimal instead. Every other
// value prints exactly as %v printed it, nil included ("<nil>", which readers
// treat as unset).
func ConfigString(v interface{}) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	}
	return fmt.Sprintf("%v", v)
}
