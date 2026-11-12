package workflowdef

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Result is a query's result as a run sees it: the explorer executors' shape.
type Result struct {
	Columns   []string
	Rows      []map[string]any
	RowCount  int
	Truncated bool
}

// Outcome is what the condition decided.
type Outcome struct {
	Met bool
	// Value is the number a value condition compared, nil for the others.
	Value *float64
	// Note explains a "not met" that the person might not expect.
	Note string
}

// Decision is which branch a run sends, and why the others did not.
type Decision struct {
	// Branch is the index of the first branch whose condition held, -1 when
	// none did.
	Branch int
	// Outcome is that branch's; its Value fills {{value}}.
	Outcome Outcome
	// Notes say why a branch had no value to compare and, when no branch
	// held, that nothing was sent.
	Notes []string
}

// Decide applies the branches to r top to bottom and stops at the first whose
// condition holds: a branch below it is not evaluated. An error fails the run,
// as Evaluate's does; a later branch never sends in place of one that could
// not be decided.
func Decide(branches []Branch, r Result) (Decision, error) {
	d := Decision{Branch: -1}
	label := func(i int, s string) string {
		if len(branches) > 1 {
			return fmt.Sprintf("Condition %d: %s", i+1, s)
		}
		return s
	}
	for i, b := range branches {
		out, err := Evaluate(b.Condition, r)
		if err != nil {
			return Decision{Branch: -1}, errors.New(label(i, err.Error()))
		}
		if out.Note != "" {
			d.Notes = append(d.Notes, label(i, out.Note))
		}
		if out.Met {
			d.Branch, d.Outcome = i, out
			return d, nil
		}
	}
	if len(branches) > 1 {
		d.Notes = append(d.Notes, "No condition was true, so nothing was sent.")
	} else {
		d.Notes = append(d.Notes, "The condition was not true, so nothing was sent.")
	}
	return d, nil
}

// Evaluate applies c to r. An error means the condition could not be decided
// (a value that is not a number), which fails the run: guessing would either
// send a false alarm or hide a real one.
func Evaluate(c Condition, r Result) (Outcome, error) {
	switch c.Type {
	case ConditionAlways, "":
		return Outcome{Met: true}, nil
	case ConditionRowsExist:
		return Outcome{Met: r.RowCount > 0}, nil
	case ConditionNoRows:
		return Outcome{Met: r.RowCount == 0}, nil
	case ConditionValue:
		if len(r.Rows) == 0 {
			return Outcome{Note: "The query returned no rows, so there was no value to compare."}, nil
		}
		col, ok := resolveColumn(c.Column, r)
		if !ok {
			return Outcome{}, fmt.Errorf("the result has no column %q to compare; its columns are %s",
				c.Column, strings.Join(r.Columns, ", "))
		}
		raw := r.Rows[0][col]
		if raw == nil {
			return Outcome{Note: fmt.Sprintf("%s was empty (NULL) in the first row, so there was no value to compare.", col)}, nil
		}
		v, ok := asNumber(raw)
		if !ok {
			return Outcome{}, fmt.Errorf("%s in the first row is not a number, so it cannot be compared with %s",
				col, strconv.FormatFloat(c.Threshold, 'f', -1, 64))
		}
		return Outcome{Met: compare(v, c.Op, c.Threshold), Value: &v}, nil
	default:
		return Outcome{}, fmt.Errorf("unknown condition %q", c.Type)
	}
}

// resolveColumn finds the column a condition or a placeholder names: an exact
// match, else a case-insensitive one. An empty name is the first column.
func resolveColumn(name string, r Result) (string, bool) {
	if name == "" {
		if len(r.Columns) > 0 {
			return r.Columns[0], true
		}
		return "", false
	}
	for _, c := range r.Columns {
		if c == name {
			return c, true
		}
	}
	for _, c := range r.Columns {
		if strings.EqualFold(c, name) {
			return c, true
		}
	}
	return "", false
}

func compare(v float64, op string, t float64) bool {
	switch op {
	case ">":
		return v > t
	case ">=":
		return v >= t
	case "<":
		return v < t
	case "<=":
		return v <= t
	case "=":
		return v == t
	case "!=":
		return v != t
	}
	return false
}

// asNumber reads a numeric cell. Drivers return NUMERIC and DECIMAL as text
// and BigQuery returns every number as text, so text that parses counts.
func asNumber(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	case json.Number:
		p, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = p
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = p
	case []byte:
		p, err := strconv.ParseFloat(strings.TrimSpace(string(n)), 64)
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// cellText renders one cell for a message.
func cellText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case time.Time:
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}
