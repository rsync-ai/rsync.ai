package transforms

import (
	"errors"
	"fmt"
	"strings"
)

// Per-step mask guard
//
// SnapshotPlaintext / VerifyNoResidualPlaintext compare a whole batch with the
// output of a whole chain, anywhere in the output. Wrapped around ONE mask_pii
// step that is the wrong question twice over: two customers who share a value
// ("London") would read as a leak of each other's, and a `username` column
// that happens to equal the masked `email` is a column the user never asked to
// mask. The batch executor and the CDC sink run the guard per step instead:
//
//	snap, err := SnapshotMaskStep(rows, step)  // BEFORE the step runs
//	out, err := <the mask step>
//	targets, err := VerifyMaskStep(snap, out)
//
// The question it answers is narrow and exact: does any place a target covers
// still hold a value that the SAME row had under a target before the step? A
// column target covers every key named like it, at any depth and in any case
// (so a top-level mask that cannot reach a nested `profile.email` is caught);
// a path target covers that exact path. A mask rewrites a value to "***", a
// prefixed digest or a partial form, or removes the key, so a covered place
// that still holds its input value means the mask did not apply there.
//
// The result names mask TARGETS (the user's config), never output paths: a
// document that uses data as keys would put that data in a path, and this
// error reaches run history, the DLQ and the AI diagnosis. The floor is the
// one VerifyNoResidualPlaintext uses (D-T7).

// MaskStepSnapshot holds, in memory only and per row, the values one mask
// step's targets cover. Build it with SnapshotMaskStep.
type MaskStepSnapshot struct {
	// shape carries the targets; its values set is swapped per row.
	shape *PlaintextSnapshot
	// pathNames are the dotted path targets, index-aligned with shape.paths.
	pathNames []string
	// leafNames maps a lower-cased column leaf to the leaf as configured.
	leafNames map[string]string
	// order is every target name in config order, deduped.
	order []string
	rows  []map[string]struct{}
}

// Rows is the number of input rows the snapshot was taken from.
func (s *MaskStepSnapshot) Rows() int { return len(s.rows) }

// Values is the number of values collected across all rows (a count, never
// the values). Zero means the guard has nothing to check.
func (s *MaskStepSnapshot) Values() int {
	n := 0
	for _, r := range s.rows {
		n += len(r)
	}
	return n
}

// SnapshotMaskStep collects, per row, the values the mask step's targets
// cover. Call it BEFORE the step runs. A step that is not an enabled mask_pii
// yields a snapshot with nothing to check.
func SnapshotMaskStep(in []Row, step CanonicalTransform) (*MaskStepSnapshot, error) {
	targets, err := MaskTargets([]CanonicalTransform{step})
	if err != nil {
		return nil, err
	}
	shape, err := SnapshotPlaintext(nil, []CanonicalTransform{step})
	if err != nil {
		return nil, err
	}
	snap := &MaskStepSnapshot{
		shape:     shape,
		leafNames: map[string]string{},
		rows:      make([]map[string]struct{}, len(in)),
	}
	seen := map[string]struct{}{}
	for _, t := range targets {
		name := t.Column
		if t.Path != "" {
			name = t.Path
			snap.pathNames = append(snap.pathNames, t.Path)
		} else if _, dup := snap.leafNames[strings.ToLower(t.Column)]; !dup {
			snap.leafNames[strings.ToLower(t.Column)] = t.Column
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			snap.order = append(snap.order, name)
		}
	}
	if len(shape.leaves) == 0 && len(shape.paths) == 0 {
		return snap, nil
	}
	for i, row := range in {
		shape.values = map[string]struct{}{}
		shape.collectRow(row)
		if len(shape.values) > 0 {
			snap.rows[i] = shape.values
		}
	}
	shape.values = map[string]struct{}{}
	return snap, nil
}

// VerifyMaskStep checks the step's output row by row against the snapshot.
// It returns the mask targets (config names, in a stable order) under which a
// row still holds one of its own input values, and an error wrapping
// ErrResidualPlaintext naming them and the number of rows affected. A nil
// snapshot or a row count that changed is an error too: a mask is 1:1, and a
// guard that cannot line rows up fails closed rather than passing.
func VerifyMaskStep(snap *MaskStepSnapshot, out []Row) ([]string, error) {
	if snap == nil {
		return nil, errors.New("mask step guard: nil snapshot (SnapshotMaskStep must run before the mask step)")
	}
	if len(out) != len(snap.rows) {
		return nil, fmt.Errorf("mask step guard: the mask step returned %d row(s) for %d input row(s)", len(out), len(snap.rows))
	}
	hit := map[string]struct{}{}
	rowsHit := 0
	for i, vals := range snap.rows {
		if len(vals) == 0 {
			continue
		}
		s := snap.shape
		s.values = vals
		rowHit := false
		m := map[string]interface{}(out[i])
		if len(s.leaves) > 0 {
			if s.verifyLeavesMap(m, snap.leafNames, hit) {
				rowHit = true
			}
		}
		for pi, segs := range s.paths {
			if s.anyAtPath(m, segs) {
				hit[snap.pathNames[pi]] = struct{}{}
				rowHit = true
			}
		}
		if rowHit {
			rowsHit++
		}
	}
	snap.shape.values = map[string]struct{}{}
	if len(hit) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(hit))
	for _, name := range snap.order {
		if _, ok := hit[name]; ok {
			names = append(names, name)
		}
	}
	return names, fmt.Errorf("%w: mask target(s) %s still hold their input value in %d of %d row(s); "+
		"a column mask on a nested document needs \"deep\": true, or name the field with \"path\"",
		ErrResidualPlaintext, quoteJoin(names), rowsHit, len(snap.rows))
}

func quoteJoin(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

// verifyLeavesMap walks m at any depth; under every key named like a column
// target it reports whether the subtree still holds a value of this row, and
// records that target's configured name in hit.
func (s *PlaintextSnapshot) verifyLeavesMap(m map[string]interface{}, names map[string]string, hit map[string]struct{}) bool {
	found := false
	for k, v := range m {
		if _, ok := s.leaves[strings.ToLower(k)]; ok {
			if s.anyScalar(v) {
				hit[names[strings.ToLower(k)]] = struct{}{}
				found = true
			}
			continue
		}
		if s.verifyLeavesIn(v, names, hit) {
			found = true
		}
	}
	return found
}

func (s *PlaintextSnapshot) verifyLeavesIn(v interface{}, names map[string]string, hit map[string]struct{}) bool {
	found := false
	switch t := v.(type) {
	case map[string]interface{}:
		found = s.verifyLeavesMap(t, names, hit)
	case Row:
		found = s.verifyLeavesMap(map[string]interface{}(t), names, hit)
	case []interface{}:
		for _, el := range t {
			if s.verifyLeavesIn(el, names, hit) {
				found = true
			}
		}
	case []map[string]interface{}:
		for _, el := range t {
			if s.verifyLeavesMap(el, names, hit) {
				found = true
			}
		}
	}
	return found
}

// anyAtPath reports whether the scalars at segs beneath v (walked the way
// collectAtPath collects) hold a value of this row.
func (s *PlaintextSnapshot) anyAtPath(v interface{}, segs []string) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		return s.anyMapAtPath(t, segs)
	case Row:
		return s.anyMapAtPath(map[string]interface{}(t), segs)
	case []interface{}:
		for _, el := range t {
			if s.anyAtPath(el, segs) {
				return true
			}
		}
	case []map[string]interface{}:
		for _, el := range t {
			if s.anyMapAtPath(el, segs) {
				return true
			}
		}
	}
	return false
}

func (s *PlaintextSnapshot) anyMapAtPath(m map[string]interface{}, segs []string) bool {
	child, ok := m[segs[0]]
	if !ok {
		return false
	}
	if len(segs) == 1 {
		return s.anyScalar(child)
	}
	return s.anyAtPath(child, segs[1:])
}

// anyScalar reports whether any scalar in v's subtree is a collected value.
func (s *PlaintextSnapshot) anyScalar(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, el := range t {
			if s.anyScalar(el) {
				return true
			}
		}
	case Row:
		for _, el := range t {
			if s.anyScalar(el) {
				return true
			}
		}
	case []interface{}:
		for _, el := range t {
			if s.anyScalar(el) {
				return true
			}
		}
	case []map[string]interface{}:
		for _, el := range t {
			if s.anyScalar(el) {
				return true
			}
		}
	default:
		k, ok := residualKey(v, false)
		if !ok {
			return false
		}
		_, hit := s.values[k]
		return hit
	}
	return false
}

// GuardMaskStep runs one mask_pii step between a per-row snapshot and its
// verification. mask is the step itself, however the caller applies it. A
// value left where a target reaches fails the step with an error wrapping
// ErrResidualPlaintext; the step's output is returned only when it is clean.
func GuardMaskStep(in []Row, step CanonicalTransform, mask func([]Row) ([]Row, error)) ([]Row, error) {
	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		return nil, err
	}
	out, err := mask(in)
	if err != nil {
		return nil, err
	}
	if _, err := VerifyMaskStep(snap, out); err != nil {
		return nil, err
	}
	return out, nil
}
