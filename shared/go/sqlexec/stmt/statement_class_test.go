package stmt

import "testing"

// Permits is the check a write runs against the class its caller was allowed: a
// statement of a higher class, a blocked one, or an allowance that is no class at
// all is refused.
func TestPermits(t *testing.T) {
	all := []StatementClass{ClassRead, ClassDMLWrite, ClassDDL, ClassDestructive, ClassBlocked, ClassUnknown, "", "nonsense"}
	allows := map[StatementClass][]StatementClass{
		ClassRead:        {ClassRead},
		ClassDMLWrite:    {ClassRead, ClassDMLWrite, ClassDDL},
		ClassDDL:         {ClassRead, ClassDMLWrite, ClassDDL},
		ClassDestructive: {ClassRead, ClassDMLWrite, ClassDDL, ClassDestructive, ClassUnknown},
		ClassUnknown:     {ClassRead, ClassDMLWrite, ClassDDL, ClassDestructive, ClassUnknown},
	}
	for _, allowed := range all {
		ok := map[StatementClass]bool{}
		for _, c := range allows[allowed] {
			ok[c] = true
		}
		for _, class := range all {
			if got := Permits(allowed, class); got != ok[class] {
				t.Errorf("Permits(%q, %q) = %v, want %v", allowed, class, got, ok[class])
			}
		}
	}
}
