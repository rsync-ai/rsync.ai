package assistant

import (
	"encoding/json"
	"fmt"
	"strings"
)

// fitResult cuts a tool result down to at most max characters, keeping what a
// count needs. A tool result that is too long is a JSON object with a list
// too long to read; its totals, pagination and counts are small and are what
// a "how many" question needs. So the longest list loses rows from its end
// until the result fits, then the next longest if that is not enough, and a
// "trimmed" note says how many rows are shown of how many. Every other key is
// kept as it was, numbers exact. A result that cannot be cut to fit (text, a
// top-level array, an object whose size is not in a list) is refused: it is
// never passed on cut mid-value.
func fitResult(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, true
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil || dec.More() {
		return "", false
	}
	if _, taken := obj["trimmed"]; taken {
		return "", false
	}
	var notes []string
	for {
		key, rows := longestList(obj)
		if rows == nil {
			return "", false
		}
		// The most rows that still fit: rows[:lo] fits, rows[:lo+1] does
		// not. A list emptied to [] may still not be enough; the next one is
		// cut then.
		fits := func(n int) (string, bool) {
			obj[key] = rows[:n]
			obj["trimmed"] = strings.Join(append(notes, trimNote(key, n, len(rows))), " ")
			out, err := json.Marshal(obj)
			return string(out), err == nil && len(out) <= max
		}
		lo, hi := 0, len(rows)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if _, ok := fits(mid); ok {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if out, ok := fits(lo); ok {
			return out, true
		}
		notes = append(notes, trimNote(key, 0, len(rows)))
		obj[key] = []any{}
	}
}

// longestList is the top-level list with rows whose JSON is the longest.
func longestList(obj map[string]any) (string, []any) {
	var key string
	var rows []any
	size := -1
	for k, v := range obj {
		l, ok := v.([]any)
		if !ok || len(l) == 0 {
			continue
		}
		raw, err := json.Marshal(l)
		if err != nil {
			continue
		}
		if len(raw) > size || (len(raw) == size && k < key) {
			key, rows, size = k, l, len(raw)
		}
	}
	return key, rows
}

func trimNote(key string, kept, of int) string {
	if kept == 0 {
		return fmt.Sprintf("None of the %d %s fit here.", of, key)
	}
	return fmt.Sprintf("Only the first %d of the %d %s fit here; every total and count in this result still covers all of them.", kept, of, key)
}
