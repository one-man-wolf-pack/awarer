package run

import (
	"fmt"
	"strconv"
	"strings"

	"awarer/internal/domain/runcache"
)

// EnvRequirement is the selected [[run.required_env]] declaration for one invocation
// together with the check taken from the invocation's own assembled environment.
type EnvRequirement struct {
	// Index is the declaration's position in the effective run.required_env list.
	Index int
	Argv  []string
	CWD   string
	Check runcache.RequirementCheck
}

// Satisfied reports whether every required name was set to a nonempty value.
func (r EnvRequirement) Satisfied() bool { return r.Check.Satisfied() }

// selectRequirement resolves the declaration selecting req's exact argv and cwd. The
// names it returns join the environment assembly; the check is attached once the
// assembly has looked them up.
func selectRequirement(req Request) (names []string, sel *EnvRequirement) {
	d, i, ok := req.Config.Run.SelectRequiredEnv(req.Argv, req.CWD.String())
	if !ok {
		return nil, nil
	}
	return d.Names, &EnvRequirement{Index: i, Argv: d.Argv, CWD: d.CWD}
}

// EnvRequirementError is run's refusal of an invocation whose selected command
// environment requirement is unmet. It is an awa failure of an execution precondition:
// configuration is valid and nothing ran, so it is neither a config error, a usage
// error, nor a child outcome. It carries names and states only, never values.
type EnvRequirementError struct {
	Requirement EnvRequirement
}

// maxReportedUnmet bounds how many failing names a diagnostic lists; the remainder is
// reported as a count.
const maxReportedUnmet = 8

// UnmetSummary renders the failing names and their states, bounded, e.g.
// `NUGET_PACKAGES is unset, TOKEN_DIR is empty, 3 more`. name renders one configured
// name for the target surface, so a presentation layer applies its own safe escaping.
func (r EnvRequirement) UnmetSummary(name func(string) string) string {
	unmet := r.Check.Unmet()
	shown := unmet
	if len(shown) > maxReportedUnmet {
		shown = shown[:maxReportedUnmet]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, s := range shown {
		parts = append(parts, fmt.Sprintf("%s is %s", name(s.Name), s.Presence))
	}
	if extra := len(unmet) - len(shown); extra > 0 {
		parts = append(parts, fmt.Sprintf("%d more", extra))
	}
	return strings.Join(parts, ", ")
}

func (e *EnvRequirementError) Error() string {
	return fmt.Sprintf("run refused before execution: run.required_env[%d] requires a nonempty caller value: %s; set it in the environment that starts awa (config holds names only, never values)",
		e.Requirement.Index, e.Requirement.UnmetSummary(strconv.Quote))
}
