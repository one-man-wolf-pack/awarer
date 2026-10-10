package cli

import (
	"sync"

	"awarer/internal/domain/pattern"
	"awarer/internal/output"
)

type patternWarner = func(pattern.Diagnostic)

// A command scans more than once, so each walk reports the same rules again.
func warnPatterns(w *output.Writer) patternWarner {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(d pattern.Diagnostic) {
		line := "warning: " + d.String()
		mu.Lock()
		defer mu.Unlock()
		if seen[line] {
			return
		}
		seen[line] = true
		w.Diagnostic(line)
	}
}
