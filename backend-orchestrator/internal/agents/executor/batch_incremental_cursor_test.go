package executor

import (
	"encoding/json"
	"testing"
)

// maxCursorValue backs the PK high-water that the incremental delta predicate
// is built from (INCREMENTAL.md §5). Getting it wrong in the "too high"
// direction skips rows; too low only costs a redundant upsert. These cases pin
// both the ordering and the deliberate fall-back-to-newer behavior.
func TestMaxCursorValue(t *testing.T) {
	cases := []struct {
		name string
		a    interface{}
		b    interface{}
		want interface{}
	}{
		{"both nil", nil, nil, nil},
		{"nil incumbent adopts new", nil, float64(7), float64(7)},
		{"nil new keeps incumbent", float64(7), nil, float64(7)},

		// Numeric PKs arrive as float64 after a JSON hop.
		{"numeric ascending", float64(3), float64(9), float64(9)},
		{"numeric descending keeps high-water", float64(9), float64(3), float64(9)},
		{"numeric equal keeps incumbent", float64(5), float64(5), float64(5)},
		{"mixed int and float64", 3, float64(9), float64(9)},
		{"json.Number", json.Number("100"), json.Number("42"), json.Number("100")},

		// UUID / text PKs compare lexicographically.
		{"string ascending", "aaa", "bbb", "bbb"},
		{"string descending keeps high-water", "bbb", "aaa", "bbb"},

		// Incomparable pairs fall back to the newer value — exactly what the
		// executor did before the high-water existed.
		{"string vs numeric falls back to new", "abc", float64(5), float64(5)},
		{"numeric vs string falls back to new", float64(5), "abc", "abc"},
		{"unknown type falls back to new", map[string]interface{}{}, float64(5), float64(5)},

		// A composite key's cursor is a list (sql_paging.py), compared column
		// by column. KI-DB-BATCH-PAGING-CAN-SKIP-ROWS.
		{"composite ascending on a later column", cursorList(1, 2), cursorList(1, 3), cursorList(1, 3)},
		{"composite descending keeps high-water", cursorList(2, 1), cursorList(1, 9), cursorList(2, 1)},
		{"composite equal", cursorList(1, 2), cursorList(1, 2), cursorList(1, 2)},
		{"composite text column", cursorList("a", 9), cursorList("b", 0), cursorList("b", 0)},
		{"composite incomparable column falls back to new", cursorList(1, "x"), cursorList(1, 2), cursorList(1, 2)},
		{"pre-composite scalar checkpoint adopts the list", float64(1), cursorList(1, 2), cursorList(1, 2)},
		{"list then scalar falls back to new", cursorList(1, 2), float64(1), float64(1)},

		// A keyless table's cursor is a row locator: a position, never a key.
		{"locator never becomes a high-water", nil, locatorCursor("(0,2)"), nil},
		{"locator keeps the key high-water", float64(7), locatorCursor("(0,2)"), float64(7)},
		{"locator incumbent gives way to a key", locatorCursor("(0,2)"), float64(7), float64(7)},

		// A warehouse sweep's cursor is a snapshot point and an offset: a
		// position, never a key. KI-WAREHOUSE-BATCH-PAGING-UNORDERED.
		{"snapshot never becomes a high-water", nil, snapshotCursor(1.7e12, 8), nil},
		{"snapshot keeps the key high-water", float64(7), snapshotCursor(1.7e12, 8), float64(7)},
		{"unpinned snapshot cursor keeps the key high-water", float64(7), snapshotCursor(nil, 8), float64(7)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maxCursorValue(tc.a, tc.b)
			if !jsonEqual(got, tc.want) {
				t.Fatalf("maxCursorValue(%#v, %#v) = %#v, want %#v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// A completed sweep that only picked up an UPDATE to a low-PK row ends BELOW
// the previous high-water. Promoting that lower value would make the next run
// re-read the table's entire tail, so the high-water must not regress.
func TestMaxCursorValueDoesNotRegressOnUpdateOnlySweep(t *testing.T) {
	highWater := interface{}(float64(100))
	// Sweep returned only row id=5 (an update); cursor ends at 5.
	highWater = maxCursorValue(highWater, float64(5))
	if !jsonEqual(highWater, float64(100)) {
		t.Fatalf("high-water regressed to %#v, want 100", highWater)
	}
	// A later sweep that inserts id=120 does advance it.
	highWater = maxCursorValue(highWater, float64(120))
	if !jsonEqual(highWater, float64(120)) {
		t.Fatalf("high-water = %#v, want 120", highWater)
	}
}

func TestCursorAsFloat(t *testing.T) {
	numeric := []interface{}{float64(1), float32(1), int(1), int32(1), int64(1), json.Number("1")}
	for _, v := range numeric {
		if f, ok := cursorAsFloat(v); !ok || f != 1 {
			t.Fatalf("cursorAsFloat(%#v) = (%v, %v), want (1, true)", v, f, ok)
		}
	}
	nonNumeric := []interface{}{"1", nil, map[string]interface{}{}, json.Number("not-a-number")}
	for _, v := range nonNumeric {
		if _, ok := cursorAsFloat(v); ok {
			t.Fatalf("cursorAsFloat(%#v) reported numeric, want false", v)
		}
	}
}

// cursorList and locatorCursor build cursors the way they arrive after the
// JSON hop.
func cursorList(vs ...interface{}) []interface{} {
	out := make([]interface{}, len(vs))
	for i, v := range vs {
		if n, ok := v.(int); ok {
			v = float64(n)
		}
		out[i] = v
	}
	return out
}

func locatorCursor(v string) map[string]interface{} {
	return map[string]interface{}{"locator": v}
}

func snapshotCursor(point interface{}, offset float64) map[string]interface{} {
	return map[string]interface{}{"snapshot": point, "offset": offset}
}

func jsonEqual(a, b interface{}) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
