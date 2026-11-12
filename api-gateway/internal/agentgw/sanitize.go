package agentgw

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// maxStringRunes caps every string a tool returns. Names and descriptions are
// written by people and by source systems, so a table name can be as long, and
// say whatever, its author liked.
const maxStringRunes = 500

// identRe is the shape of an enum-like field (a status, a connector type, a
// mode). Anything else in such a field is dropped rather than passed on.
var identRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:\-]{0,63}$`)

func ident(s string) string {
	if s = strings.TrimSpace(s); identRe.MatchString(s) {
		return s
	}
	return ""
}

// cleanText removes control and format characters (bidi overrides, zero-width
// and tag characters, which can hide text from the person reading a
// transcript) and caps the length. Whitespace controls become a space.
func cleanText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxStringRunes {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// sanitize turns a tool's typed output into the value the client receives:
// every string cleaned, numbers kept exact. The output types are the allowlist
// of keys; this pass only makes the values inert.
func sanitize(out any) (any, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return cleanValue(v), nil
}

func cleanValue(v any) any {
	switch t := v.(type) {
	case string:
		return cleanText(t)
	case []any:
		for i := range t {
			t[i] = cleanValue(t[i])
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = cleanValue(e)
		}
		return t
	default:
		return v
	}
}
