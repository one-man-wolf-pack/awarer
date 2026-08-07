package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
)

func TestClassifyGeneratedArtifact(t *testing.T) {
	generated := map[string]string{
		"obj/generated.cache":       "obj/",
		"build/app.js":              "build/",
		"target/classes/Main.class": "target/",
		"node_modules/x/index.js":   "node_modules/",
		"./dist/bundle.js":          "dist/",
		"pkg/foo.o":                 "*.o",
		"src/Main.class":            "*.class",
		// A nested generated dir suggests the full prefix, not the bare segment: a bare
		// "obj/" would name a different, top-level directory than "src/obj/".
		"src/obj/generated.o": "src/obj/",
	}
	for path, wantSuggestion := range generated {
		hint, ok := runcache.ClassifyGeneratedArtifact(path)
		if !ok {
			t.Errorf("ClassifyGeneratedArtifact(%q) = not generated, want a hint", path)
			continue
		}
		if hint.Path != path {
			t.Errorf("hint.Path = %q, want the real path %q (a hint must never hide the path)", hint.Path, path)
		}
		if hint.Suggestion != wantSuggestion {
			t.Errorf("ClassifyGeneratedArtifact(%q) suggestion = %q, want %q", path, hint.Suggestion, wantSuggestion)
		}
		if !strings.Contains(hint.Message, "extra_excludes") {
			t.Errorf("hint message should point at [run].extra_excludes, got %q", hint.Message)
		}
	}
}

func TestClassifyGeneratedArtifactDeclinesSource(t *testing.T) {
	for _, path := range []string{
		"src/main.go",
		"README.md",
		"internal/app/run/run.go",
		"cmd/awa/main.go",
		"",
	} {
		if hint, ok := runcache.ClassifyGeneratedArtifact(path); ok {
			t.Errorf("ClassifyGeneratedArtifact(%q) = hint %+v, want declined (a source path is not generated)", path, hint)
		}
	}
}
