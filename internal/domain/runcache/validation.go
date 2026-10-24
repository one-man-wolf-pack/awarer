package runcache

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"awarer/internal/domain/config"
)

const ValidationResultOnly = "result-only"

// ValidationScope holds only the waived directories because the policy's argv and cwd
// selectors are the key's own command and cwd.
type ValidationScope struct {
	// Label is the policy's diagnostic name. It is retained for presentation and
	// deliberately kept out of the key and of CompareKeyInputs: renaming a policy
	// must not invalidate the results it already permits.
	Label          string
	UnobservedDirs []string
}

func NewValidationScope(label string, dirs []string) (ValidationScope, error) {
	v := ValidationScope{Label: label, UnobservedDirs: slices.Clone(dirs)}
	slices.Sort(v.UnobservedDirs)
	if err := v.Validate(); err != nil {
		return ValidationScope{}, err
	}
	return v, nil
}

func (v ValidationScope) Validate() error {
	if v.Label == "" || v.Label != strings.TrimSpace(v.Label) || strings.ContainsAny(v.Label, "\x00\r\n") {
		return fmt.Errorf("validation policy label %q is invalid", v.Label)
	}
	if len(v.UnobservedDirs) == 0 {
		return fmt.Errorf("validation policy %q waives no directory", v.Label)
	}
	for i, d := range v.UnobservedDirs {
		if p := config.ValidationDirProblem(d); p != "" {
			return fmt.Errorf("validation policy %q unobserved directory %d: %s", v.Label, i, p)
		}
		if i > 0 {
			prev := v.UnobservedDirs[i-1]
			if prev >= d {
				return fmt.Errorf("validation policy %q unobserved directories are not sorted and unique", v.Label)
			}
		}
		for _, other := range v.UnobservedDirs[:i] {
			if config.PathWithin(d, other) {
				return fmt.Errorf("validation policy %q unobserved directory %q lies within %q", v.Label, d, other)
			}
		}
	}
	return nil
}

func sameValidationSemantics(a, b *ValidationScope) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.UnobservedDirs, b.UnobservedDirs)
}

func validationString(v *ValidationScope) string {
	if v == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s unobserved=%v", ValidationResultOnly, v.UnobservedDirs)
}

// putValidation appends the keyed facts of a selected policy. An unselected run
// appends nothing, so an ordinary key keeps its predecessor bytes; because every
// earlier field is length-prefixed, the suffix's presence alone separates the two.
func putValidation(b *bytes.Buffer, v *ValidationScope) {
	if v == nil {
		return
	}
	putString(b, "validation")
	putString(b, ValidationResultOnly)
	putList(b, v.UnobservedDirs)
}
