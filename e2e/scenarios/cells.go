package scenarios

import (
	"fmt"
	"testing"
)

// forEachCell enumerates the Cartesian product of axes and invokes
// body once per cell, each call nested under t.Run for every axis
// value in turn. The cell map passed to body is keyed by Axis.Name
// and contains exactly one entry per axis.
//
// When axes is empty, body is called once on t directly with an
// empty cell — no t.Run wrapping. This keeps simple no-axis
// backends (the mock) from gaining a redundant sub-test layer.
//
// forEachCell fatals if any axis has an empty Values() slice (that
// would otherwise silently skip the whole suite) or if two axes
// share a Name (the cell map indexes by name, so duplicates would
// silently collapse dimensions).
func forEachCell(t *testing.T, axes []Axis, body func(*testing.T, map[string]string)) {
	t.Helper()
	if len(axes) == 0 {
		body(t, map[string]string{})
		return
	}
	if err := validateAxisNames(axes); err != nil {
		t.Fatalf("forEachCell: %v", err)
	}
	enumerate(t, axes, map[string]string{}, body)
}

func enumerate(t *testing.T, axes []Axis, acc map[string]string, body func(*testing.T, map[string]string)) {
	t.Helper()
	if len(axes) == 0 {
		// Defensive copy: the caller is about to start scenario
		// sub-tests and may stash the cell; reusing the working map
		// across sibling cells would mutate prior cells' stored
		// state.
		cell := make(map[string]string, len(acc))
		for k, v := range acc {
			cell[k] = v
		}
		body(t, cell)
		return
	}
	axis := axes[0]
	values := axis.Values()
	if len(values) == 0 {
		t.Fatalf("axis %q has no values", axis.Name())
	}
	for _, v := range values {
		v := v
		t.Run(v, func(t *testing.T) {
			acc[axis.Name()] = v
			enumerate(t, axes[1:], acc, body)
			delete(acc, axis.Name())
		})
	}
}

// validateAxisNames returns an error if any Axis.Name is empty or if
// two axes share a name. The cell maps used by forEachCell are keyed
// by Axis.Name, so duplicates would silently overwrite each other and
// collapse dimensions; empty names produce unreadable cell maps and
// ambiguous Cell() contracts.
func validateAxisNames(axes []Axis) error {
	seen := make(map[string]struct{}, len(axes))
	for _, a := range axes {
		name := a.Name()
		if name == "" {
			return fmt.Errorf("axis with empty Name()")
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate axis Name() %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}
