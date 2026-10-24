package config

import (
	"fmt"
	"slices"
	"strings"
)

type ValidationPolicy struct {
	Name           string
	Argv           []string
	CWD            string
	UnobservedDirs []string
}

func (p ValidationPolicy) clone() ValidationPolicy {
	p.Argv = slices.Clone(p.Argv)
	p.UnobservedDirs = slices.Clone(p.UnobservedDirs)
	return p
}

func cloneValidation(ps []ValidationPolicy) []ValidationPolicy {
	if ps == nil {
		return nil
	}
	out := make([]ValidationPolicy, len(ps))
	for i, p := range ps {
		out[i] = p.clone()
	}
	return out
}

func validationEqual(a, b []ValidationPolicy) bool {
	return slices.EqualFunc(a, b, func(x, y ValidationPolicy) bool {
		return x.Name == y.Name && x.CWD == y.CWD &&
			slices.Equal(x.Argv, y.Argv) && slices.Equal(x.UnobservedDirs, y.UnobservedDirs)
	})
}

// SelectValidation can return the first match: Validate rejects two policies with one
// selector.
func (r Run) SelectValidation(argv []string, cwd string) (ValidationPolicy, bool) {
	for _, p := range r.Validation {
		if p.CWD == cwd && slices.Equal(p.Argv, argv) {
			return p.clone(), true
		}
	}
	return ValidationPolicy{}, false
}

// ValidationDirProblem treats a single component, unlike an effect-root selector, as
// the root's own child, never a basename at every depth.
func ValidationDirProblem(dir string) string {
	switch {
	case dir == "":
		return "must not be empty"
	case dir != strings.TrimSpace(dir):
		return fmt.Sprintf("%q must not have leading or trailing whitespace", dir)
	case strings.ContainsAny(dir, "\\:\x00\r\n"):
		return fmt.Sprintf(`%q must not contain a backslash, volume separator, NUL, or line break: "/" is the path separator on every platform`, dir)
	case strings.HasPrefix(dir, "/"):
		return fmt.Sprintf("%q must be relative to the project root", dir)
	case strings.HasSuffix(dir, "/"):
		return fmt.Sprintf("%q must not end in a slash: name the directory itself", dir)
	}
	for _, comp := range strings.Split(dir, "/") {
		switch {
		case comp == "":
			return fmt.Sprintf("%q must not have an empty path component", dir)
		case comp == "." || comp == "..":
			return fmt.Sprintf("%q must be a clean project-relative directory below the root, without a %q or %q component", dir, ".", "..")
		case slices.Contains(protectedExcludes, comp):
			return fmt.Sprintf("%q must not select protected state: awa never observes a %q component", dir, comp)
		}
	}
	return ""
}

func PathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func validationCWDProblem(cwd string) string {
	switch cwd {
	case "":
		return `must not be empty: use "." for the project root`
	case ".":
		return ""
	}
	return ValidationDirProblem(cwd)
}

func validationProblems(policies []ValidationPolicy) []string {
	var probs []string
	names := make(map[string]int, len(policies))
	type selector struct{ cwd, argv string }
	selectors := make(map[selector]int, len(policies))
	for i, p := range policies {
		at := fmt.Sprintf("run.validation[%d]", i)
		switch {
		case p.Name == "":
			probs = append(probs, at+".name must not be empty")
		case p.Name != strings.TrimSpace(p.Name) || strings.ContainsAny(p.Name, "\x00\r\n"):
			probs = append(probs, fmt.Sprintf("%s.name %q must not have surrounding whitespace, NUL, or line breaks", at, p.Name))
		default:
			if j, dup := names[p.Name]; dup {
				probs = append(probs, fmt.Sprintf("%s.name %q duplicates run.validation[%d]", at, p.Name, j))
			} else {
				names[p.Name] = i
			}
		}
		if len(p.Argv) == 0 || p.Argv[0] == "" {
			probs = append(probs, at+".argv must list the complete command, starting with a non-empty executable")
		}
		for j, a := range p.Argv {
			if strings.ContainsRune(a, 0) {
				probs = append(probs, fmt.Sprintf("%s.argv[%d] must not contain NUL", at, j))
			}
		}
		if prob := validationCWDProblem(p.CWD); prob != "" {
			probs = append(probs, fmt.Sprintf("%s.cwd %s", at, prob))
		}
		if len(p.UnobservedDirs) == 0 {
			probs = append(probs, at+".unobserved_dirs must list at least one directory")
		}
		for j, d := range p.UnobservedDirs {
			if prob := ValidationDirProblem(d); prob != "" {
				probs = append(probs, fmt.Sprintf("%s.unobserved_dirs[%d] %s", at, j, prob))
				continue
			}
			for k, earlier := range p.UnobservedDirs[:j] {
				if PathWithin(d, earlier) || PathWithin(earlier, d) {
					probs = append(probs, fmt.Sprintf("%s.unobserved_dirs[%d] %q overlaps unobserved_dirs[%d] %q: list each waived directory once", at, j, d, k, earlier))
					break
				}
			}
		}
		if len(p.Argv) > 0 {
			sel := selector{cwd: p.CWD, argv: string(encodeArgv(p.Argv))}
			if j, dup := selectors[sel]; dup {
				probs = append(probs, fmt.Sprintf("%s conflicts with run.validation[%d]: both select argv %q in cwd %q", at, j, p.Argv, p.CWD))
			} else {
				selectors[sel] = i
			}
		}
	}
	return probs
}

// encodeArgv is injective: two distinct vectors never collide the way a space-joined
// command line would.
func encodeArgv(argv []string) []byte {
	var b []byte
	for _, a := range argv {
		b = fmt.Appendf(b, "%d:%s", len(a), a)
	}
	return b
}
