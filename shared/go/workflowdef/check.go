package workflowdef

import (
	"fmt"
	"strings"
)

// checkColumnsMax caps the column names a refusal lists.
const checkColumnsMax = 40

// CheckResult checks d against what its query returned when a draft ran once:
// every {{slot}} in each branch's subject and message is a fixed variable or a
// column of the result, and a value condition names a column whose first-row
// cell is a number. The refusal names columns and nothing else, never a value:
// it goes back to the model that wrote the draft, so it can fix it.
func CheckResult(d Definition, r Result) *Invalid {
	bs := d.Branches()
	for i, b := range bs {
		if inv := checkBranch(b, r); inv != nil {
			inv.Branch = i + 1
			if s := d.CaseSend(i); s != nil {
				inv.Step = s.ID
			}
			if len(bs) > 1 {
				inv.Message = fmt.Sprintf("Condition %d: %s", i+1, inv.Message)
			}
			return inv
		}
	}
	return nil
}

func checkBranch(b Branch, r Result) *Invalid {
	for _, field := range []struct{ name, text string }{
		{"subject", b.Message.Subject},
		{"message", b.Message.Body},
	} {
		if unknown := unknownSlots(field.text, r); len(unknown) > 0 {
			return invalid(field.name, "The query returns no column for %s; its columns are %s. A slot is a "+
				"column name the SQL returns: name each number with AS, or write a slot the SQL has.",
				strings.Join(unknown, ", "), columnList(r.Columns))
		}
	}
	if b.Condition.Type != ConditionValue {
		return nil
	}
	if _, ok := resolveColumn(b.Condition.Column, r); !ok {
		return invalid("condition_column", "The query returns no column %q to compare; its columns are %s.",
			b.Condition.Column, columnList(r.Columns))
	}
	if _, err := Evaluate(b.Condition, r); err != nil {
		return invalid("condition_column", "%s. Compare a column that holds a number, such as COUNT(*) AS failed.", err.Error())
	}
	return nil
}

// unknownSlots lists each {{name}} in text that is neither a fixed variable
// nor a column of r, once each, in order.
func unknownSlots(text string, r Result) []string {
	var unknown []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if seen[name] || isFixedPlaceholder(name) {
			continue
		}
		seen[name] = true
		if _, ok := resolveColumn(name, r); !ok {
			unknown = append(unknown, "{{"+name+"}}")
		}
	}
	return unknown
}

func isFixedPlaceholder(name string) bool {
	for _, f := range fixedPlaceholders {
		if f == name {
			return true
		}
	}
	return false
}

func columnList(cols []string) string {
	if len(cols) == 0 {
		return "none"
	}
	if len(cols) <= checkColumnsMax {
		return strings.Join(cols, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(cols[:checkColumnsMax], ", "), len(cols)-checkColumnsMax)
}
