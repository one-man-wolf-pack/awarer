package config

import (
	"fmt"
	"slices"
	"strings"
)

// EnvRequirement is one [[run.required_env]] declaration: the caller variables the
// invocation with exactly this argv and execution cwd must receive, each present and
// nonempty. Naming a variable here is also the permission to pass it to the child, so
// it needs no run.env_allowlist entry. The declaration is independent of
// run.validation: it waives nothing and needs no validation policy.
type EnvRequirement struct {
	Argv  []string
	CWD   string
	Names []string
}

func (r EnvRequirement) clone() EnvRequirement {
	r.Argv = slices.Clone(r.Argv)
	r.Names = slices.Clone(r.Names)
	return r
}

func cloneRequiredEnv(rs []EnvRequirement) []EnvRequirement {
	if rs == nil {
		return nil
	}
	out := make([]EnvRequirement, len(rs))
	for i, r := range rs {
		out[i] = r.clone()
	}
	return out
}

func requiredEnvEqual(a, b []EnvRequirement) bool {
	return slices.EqualFunc(a, b, func(x, y EnvRequirement) bool {
		return x.CWD == y.CWD && slices.Equal(x.Argv, y.Argv) && slices.Equal(x.Names, y.Names)
	})
}

// SelectRequiredEnv returns the declaration selecting the invocation and its index in
// run.required_env. Selection is the literal equality validation policies use: the
// complete argv element for element and the exact project-relative cwd, with no
// prefix, alias or shell parsing. Validate rejects two declarations with one
// selector, so the first match is the only one.
func (r Run) SelectRequiredEnv(argv []string, cwd string) (EnvRequirement, int, bool) {
	for i, d := range r.RequiredEnv {
		if d.CWD == cwd && slices.Equal(d.Argv, argv) {
			return d.clone(), i, true
		}
	}
	return EnvRequirement{}, 0, false
}

// requiredEnvProblems validates run.required_env. A name follows the
// run.env_allowlist lexical rules (nonblank, no "=", compared case-insensitively so
// one config means the same on every platform) and may not be the awa-owned marker,
// which the caller cannot supply. Unlike the optional allowlist it may repeat a
// baseline or allowlisted name: requiring a variable the child already inherits is
// a precondition, not a redundant permission.
func requiredEnvProblems(decls []EnvRequirement) []string {
	var probs []string
	type selector struct{ cwd, argv string }
	selectors := make(map[selector]int, len(decls))
	for i, d := range decls {
		at := fmt.Sprintf("run.required_env[%d]", i)
		if len(d.Argv) == 0 || d.Argv[0] == "" {
			probs = append(probs, at+".argv must list the complete command, starting with a non-empty executable")
		}
		for j, a := range d.Argv {
			if strings.ContainsRune(a, 0) {
				probs = append(probs, fmt.Sprintf("%s.argv[%d] must not contain NUL", at, j))
			}
		}
		if prob := validationCWDProblem(d.CWD); prob != "" {
			probs = append(probs, fmt.Sprintf("%s.cwd %s", at, prob))
		}
		if len(d.Names) == 0 {
			probs = append(probs, at+".names must list at least one environment variable name")
		}
		seen := make(map[string]int, len(d.Names))
		for j, name := range d.Names {
			nat := fmt.Sprintf("%s.names[%d]", at, j)
			switch {
			case name == "":
				probs = append(probs, nat+" must not be empty")
				continue
			case strings.ContainsRune(name, '='):
				probs = append(probs, fmt.Sprintf("%s %q must be a name, not name=value: values come from the caller's environment, never from config", nat, name))
				continue
			case strings.ContainsRune(name, 0):
				probs = append(probs, nat+" must not contain NUL")
				continue
			case IsReservedEnvName(name):
				probs = append(probs, fmt.Sprintf("%s %q is reserved by awa: it is injected into every executed child as %s=%s, so a caller value can never satisfy it", nat, name, WrapperMarkerName, WrapperMarkerValue))
				continue
			}
			fold := strings.ToUpper(name)
			if k, dup := seen[fold]; dup {
				probs = append(probs, fmt.Sprintf("%s %q duplicates names[%d] (case-insensitively)", nat, name, k))
				continue
			}
			seen[fold] = j
		}
		if len(d.Argv) > 0 {
			sel := selector{cwd: d.CWD, argv: string(encodeArgv(d.Argv))}
			if j, dup := selectors[sel]; dup {
				probs = append(probs, fmt.Sprintf("%s conflicts with run.required_env[%d]: both select argv %q in cwd %q", at, j, d.Argv, d.CWD))
			} else {
				selectors[sel] = i
			}
		}
	}
	return probs
}
