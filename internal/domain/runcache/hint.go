package runcache

import (
	"path"
	"strings"

	"awarer/internal/domain/config"
)

// AdvisoryHint is an explicitly non-authoritative suggestion attached to a
// diagnostic. It is NOT a MismatchReason and NOT a cache decision: it never
// explains why a run was or was not reused, and acting on it is optional. Its only
// job is to teach a user how they might scope the run cache (via
// [run].extra_excludes) when a changed path looks like generated build output.
// Keeping it a distinct type from MismatchReason is deliberate — a hint can never
// be mistaken for, or promoted into, a correctness reason.
type AdvisoryHint struct {
	// Path is the actual changed path that triggered the hint. It is always the
	// real path, never hidden or rewritten, so the hint cannot conceal the change
	// that caused a miss.
	Path string
	// Suggestion is the scope pattern the user might exclude (e.g. "obj/"). It is a
	// suggestion for [run].extra_excludes, not an applied rule.
	Suggestion string
	// Message is the rendered advisory text.
	Message string
}

// generatedDirs is the authoritative set of directory names awa treats as generated
// build output, owned by config (config.GeneratedOutputDirs) so it cannot drift from
// the watched effect roots. It only drives an advisory hint here and never changes
// cache behavior or hides a path.
var generatedDirs = config.GeneratedOutputDirs()

// generatedExts is a small set of file extensions that conventionally name compiled
// or generated artifacts. Same caveat as generatedDirs: advisory only.
var generatedExts = map[string]bool{
	".o":     true,
	".obj":   true,
	".class": true,
	".pyc":   true,
	".pyo":   true,
}

// ClassifyGeneratedArtifact reports whether a changed input path looks like
// generated build output and, if so, returns an advisory hint suggesting the user
// consider excluding it from the run scope. It is conservative: it matches on a
// leading directory segment from a known set, or a known compiled-artifact
// extension, and otherwise declines (ok=false) rather than guessing. The returned
// hint always names the real path and only ever suggests — it never asserts that
// the path is not a genuine input.
func ClassifyGeneratedArtifact(p string) (AdvisoryHint, bool) {
	clean := strings.TrimSpace(p)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "" {
		return AdvisoryHint{}, false
	}
	segments := strings.Split(clean, "/")
	// Suggest the path prefix up to and including the matched directory, not the bare
	// segment: a nested "src/obj/x" must suggest excluding "src/obj/", since a bare
	// "obj/" would name a different, top-level directory.
	for i, seg := range segments {
		if generatedDirs[seg] {
			return newArtifactHint(p, strings.Join(segments[:i+1], "/")+"/"), true
		}
	}
	if ext := strings.ToLower(path.Ext(clean)); ext != "" && generatedExts[ext] {
		return newArtifactHint(p, "*"+ext), true
	}
	return AdvisoryHint{}, false
}

// newArtifactHint renders the advisory hint for a path and a suggested exclude
// pattern.
func newArtifactHint(realPath, suggestion string) AdvisoryHint {
	return AdvisoryHint{
		Path:       realPath,
		Suggestion: suggestion,
		Message: suggestion + " looks like generated build output. If it is not a " +
			"command input, consider adding it to [run].extra_excludes.",
	}
}
