package cli

import (
	"awarer/internal/infra/projfs"
	"awarer/internal/output"
)

// guardPreflight keeps the .awa/.gitignore guard healthy before a command works
// with project state. The guard is what keeps the whole state directory — captured
// output, manifests, content blobs, run metadata — out of git; awa init creates it,
// but a user or tool can delete or damage it later, so high-traffic commands
// revalidate and, because awa owns the state directory, transparently restore it.
//
// It never touches the repository-root .gitignore: awa only manages its own
// .awa/.gitignore. Doctor remains the deeper diagnostic authority (tracked-by-git,
// not-ignored, and other conditions this cheap preflight does not inspect).
//
// strict selects the failure policy. A command that is about to write more durable
// state into .awa/ (checkpoint, run) passes strict=true: if the guard is missing or
// malformed and cannot be restored, it fails loud rather than growing unprotected
// state. A read-only command (status) passes strict=false: it warns and continues,
// since it adds no new leakable state.
func guardPreflight(w *output.Writer, proj projfs.Project, strict bool) error {
	layout, err := proj.Paths()
	if err != nil {
		return err
	}
	changed, err := projfs.EnsureStateGitignore(layout)
	if err != nil {
		if strict {
			return genericErrorf("cannot protect %s from git: %v\nawa will not write state into an unguarded directory; fix the guard or run 'awa doctor'", layout.StateGitignore(), err)
		}
		w.Diagnostic("warning: could not verify the .awa/.gitignore guard: " + err.Error())
		return nil
	}
	if changed {
		w.Diagnostic("awa: restored the missing .awa/.gitignore guard (keeps .awa out of git)")
	}
	return nil
}
