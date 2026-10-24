package run

import (
	"fmt"
	"path"
	"path/filepath"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// ConflictSource decides the CLI exit class of a refused invocation.
type ConflictSource int

const (
	ConflictCLI ConflictSource = iota + 1
	ConflictConfig
	// ConflictRecorded is a stored run's recorded scope reconstructed for a current
	// assessment; it is neither a new request nor damage to the record.
	ConflictRecorded
)

type ValidationConflictError struct {
	Source  ConflictSource
	Policy  string
	Dir     string
	Origin  string
	Operand string
}

func (e *ValidationConflictError) Error() string {
	return fmt.Sprintf("validation policy %q leaves %q unobserved, but %s %q asks to observe it: an input at or below a waived directory cannot be keyed; narrow the scope or change the policy's unobserved_dirs",
		e.Policy, e.Dir, e.Origin, e.Operand)
}

// CLI operands are checked before configured defaults so their conflict is the one
// reported. A strict ancestor, including ".", is a broader scope the policy refines,
// not a conflict.
func selectValidation(req Request) (*runcache.ValidationScope, error) {
	pol, ok := req.Config.Run.SelectValidation(req.Argv, req.CWD.String())
	if !ok {
		return nil, nil
	}
	v, err := runcache.NewValidationScope(pol.Name, pol.UnobservedDirs)
	if err != nil {
		return nil, err
	}
	check := func(src ConflictSource, origin string, operands []string) error {
		for _, op := range operands {
			clean := path.Clean(filepath.ToSlash(op))
			for _, dir := range v.UnobservedDirs {
				if config.PathWithin(clean, dir) {
					return &ValidationConflictError{Source: src, Policy: pol.Name, Dir: dir, Origin: origin, Operand: op}
				}
			}
		}
		return nil
	}
	if req.EffectiveScope != nil {
		if err := check(ConflictRecorded, "the recorded include scope", req.EffectiveScope.Include); err != nil {
			return nil, err
		}
		return &v, nil
	}
	if err := check(ConflictCLI, "--scope", req.Scope.ScopeReplace); err != nil {
		return nil, err
	}
	if err := check(ConflictCLI, "--include", req.Scope.Include); err != nil {
		return nil, err
	}
	if req.Scope.ScopeReplace == nil {
		if err := check(ConflictConfig, "run.default_scope", req.Config.Run.DefaultScope); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

func waivedDirs(v *runcache.ValidationScope) []string {
	if v == nil {
		return nil
	}
	return v.UnobservedDirs
}
