package cli

import (
	"fmt"
	"strings"

	"awarer/internal/domain/runcache"
)

type validationPolicyDoc struct {
	Policy         string   `json:"policy"`
	Replay         string   `json:"replay"`
	UnobservedDirs []string `json:"unobserved_dirs"`
}

func validationPolicyView(v *runcache.ValidationScope) *validationPolicyDoc {
	if v == nil {
		return nil
	}
	return &validationPolicyDoc{
		Policy:         v.Label,
		Replay:         runcache.ValidationResultOnly,
		UnobservedDirs: append([]string{}, v.UnobservedDirs...),
	}
}

const maxValidationDirsShown = 3

func validationDirsLabel(v *runcache.ValidationScope) string {
	dirs := v.UnobservedDirs
	label := strings.Join(dirs[:min(len(dirs), maxValidationDirsShown)], ", ")
	if extra := len(dirs) - maxValidationDirsShown; extra > 0 {
		label += fmt.Sprintf(" (+%d more)", extra)
	}
	return label
}

func validationReplayLine(v *runcache.ValidationScope) string {
	return fmt.Sprintf("result only (policy %s); not observed or recreated: %s", v.Label, validationDirsLabel(v))
}

func validationPolicyLine(v *runcache.ValidationScope) string {
	return fmt.Sprintf("%s (result-only replay); not observed: %s", v.Label, validationDirsLabel(v))
}
