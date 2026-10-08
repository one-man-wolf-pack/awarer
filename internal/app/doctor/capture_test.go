package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	dom "awarer/internal/domain/doctor"
	"awarer/internal/scantest"
)

// writeRoot writes one dummy fixture file under the project root.
func (e *env) writeRoot(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(e.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureAdvice returns the candidate the capture-eligibility check names ("" when
// none) and whether it reported itself incomplete, read from the typed check result
// rather than finding prose. It also requires doctor to emit exactly the matching
// findings, so the result is the one the report is built from.
func (e *env) captureAdvice(t *testing.T) (named string, incomplete bool) {
	t.Helper()
	layout, err := e.project.Paths()
	if err != nil {
		t.Fatal(err)
	}
	check := secretCaptureCandidates(context.Background(), layout, e.cfg)
	res := e.run(t, noGit(), Request{})
	if got, want := len(findingsByCode(res, dom.CodeContentStorageEnabled)), boolCount(check.eligible != ""); got != want {
		t.Fatalf("content-storage-enabled findings = %d, want %d", got, want)
	}
	if got, want := len(findingsByCode(res, dom.CodeContentCaptureCheckIncomplete)), boolCount(check.uncertain != ""); got != want {
		t.Fatalf("content-capture-check-incomplete findings = %d, want %d", got, want)
	}
	return check.eligible, check.uncertain != ""
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestCaptureAdviceFollowsEffectiveCheckpointPolicy drives the bounded advisory through
// every policy owner that can decline a candidate's bytes. Each case is a fresh project
// with dummy candidate files; the advisory must name exactly the candidate a
// checkpoint would own bytes for — and a declined earlier candidate must not hide a
// later eligible one.
func TestCaptureAdviceFollowsEffectiveCheckpointPolicy(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		cfg   func(*config.Config)
		want  string
	}{
		{"default env selection is hash-only", map[string]string{".env": "A=1"}, nil, ""},
		{"suppressed first candidate does not hide a later one", map[string]string{".env": "A=1", ".netrc": "m"}, nil, ".netrc"},
		{"template exception is eligible", map[string]string{".env.example": "A="}, nil, ".env.example"},
		{"explicit empty selection stores env bytes", map[string]string{".env": "A=1"},
			func(c *config.Config) { c.Checkpoint.HashOnlyPatterns = []string{} }, ".env"},
		{"custom selection replaces the defaults", map[string]string{".env": "A=1", ".netrc": "m"},
			func(c *config.Config) { c.Checkpoint.HashOnlyPatterns = []string{".netrc"} }, ".env"},
		{"storage off", map[string]string{".netrc": "m"},
			func(c *config.Config) { c.Checkpoint.StoreFileContents = false }, ""},
		{"awaignore excludes the candidate", map[string]string{".netrc": "m", ".awaignore": ".netrc\n"}, nil, ""},
		{"disabled gitignore does not exclude", map[string]string{".netrc": "m", ".gitignore": ".netrc\n"}, nil, ".netrc"},
		{"enabled gitignore excludes", map[string]string{".netrc": "m", ".gitignore": ".netrc\n"},
			func(c *config.Config) { c.Scope.UseGitignore = true }, ""},
		{"configured extra exclude", map[string]string{".netrc": "m"},
			func(c *config.Config) { c.Scope.ExtraExcludes = []string{".netrc"} }, ""},
		{"narrowed scope leaves the candidate out", map[string]string{".netrc": "m"},
			func(c *config.Config) { c.Scope.Include = []string{"src"} }, ""},
		{"narrowed scope outranks an ignore rule", map[string]string{".netrc": "m", ".awaignore": ".netrc\n"},
			func(c *config.Config) { c.Scope.Include = []string{".netrc"} }, ".netrc"},
		{"large file kept hash-only", map[string]string{".netrc": strings.Repeat("m", 64)},
			func(c *config.Config) { c.Hashing.MaxFileSize = 16 }, ""},
		{"large file skipped", map[string]string{".netrc": strings.Repeat("m", 64)},
			func(c *config.Config) { c.Hashing.MaxFileSize = 16; c.Hashing.LargeFilePolicy = config.LargeFileSkip }, ""},
		{"large file stored", map[string]string{".netrc": strings.Repeat("m", 64)},
			func(c *config.Config) { c.Hashing.MaxFileSize = 16; c.Hashing.LargeFilePolicy = config.LargeFileStore }, ".netrc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			for name, content := range c.files {
				e.writeRoot(t, name, content)
			}
			if c.cfg != nil {
				c.cfg(&e.cfg)
			}
			named, incomplete := e.captureAdvice(t)
			if named != c.want {
				t.Errorf("advisory names %q, want %q", named, c.want)
			}
			if incomplete {
				t.Error("a fully inspectable project reported an incomplete capture check")
			}
		})
	}
}

// TestCaptureAdviceUsesTheRecordedPathNotTheTarget: with symlinks recorded as links a
// candidate link owns no bytes; followed, the link is captured by its own name.
func TestCaptureAdviceUsesTheRecordedPathNotTheTarget(t *testing.T) {
	scantest.RequireSymlinks(t)
	e := newEnv(t)
	e.writeRoot(t, "data/plain.txt", "m")
	scantest.Symlink(t, e.root, ".netrc", "data/plain.txt")
	if named, _ := e.captureAdvice(t); named != "" {
		t.Errorf("an unfollowed link was advised as captured (%q)", named)
	}
	e.cfg.Scope.FollowSymlinks = true
	if named, _ := e.captureAdvice(t); named != ".netrc" {
		t.Errorf("a followed link to a stored file was not advised (got %q)", named)
	}
}

// TestCaptureAdviceReadsNoCandidateContent: an unreadable candidate whose metadata is
// visible is still classified — the check never opens candidate bytes — while an
// unreadable ignore source leaves the answer unknown and is reported as such rather
// than as a pass.
func TestCaptureAdviceReadsNoCandidateContent(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permission semantics as a non-root user")
	}
	t.Run("unreadable candidate", func(t *testing.T) {
		e := newEnv(t)
		e.writeRoot(t, ".netrc", "m")
		if err := os.Chmod(filepath.Join(e.root, ".netrc"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(e.root, ".netrc"), 0o600) })
		named, incomplete := e.captureAdvice(t)
		if named != ".netrc" || incomplete {
			t.Errorf("advice = %q incomplete=%v, want .netrc classified from metadata alone", named, incomplete)
		}
	})
	t.Run("unreadable ignore source", func(t *testing.T) {
		e := newEnv(t)
		e.writeRoot(t, ".netrc", "m")
		e.writeRoot(t, ".awaignore", ".netrc\n")
		ignore := filepath.Join(e.root, ".awaignore")
		if err := os.Chmod(ignore, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ignore, 0o600) })
		res := e.run(t, noGit(), Request{})
		if got := findingsByCode(res, dom.CodeContentCaptureCheckIncomplete); len(got) != 1 {
			t.Fatalf("content-capture-check-incomplete findings = %d, want 1", len(got))
		}
		if got := findingsByCode(res, dom.CodeContentStorageEnabled); len(got) != 0 {
			t.Errorf("an uninspectable candidate was advised as captured: %v", got[0].Message())
		}
	})
}

// TestCaptureCheckCancellationIsAnInterruption: a cancelled diagnosis fails as an
// interruption instead of reporting the capture check as incomplete.
func TestCaptureCheckCancellationIsAnInterruption(t *testing.T) {
	e := newEnv(t)
	e.writeRoot(t, ".netrc", "m")
	layout, err := e.project.Paths()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := secretCaptureCandidates(ctx, layout, e.cfg); got != (captureCheck{}) {
		t.Errorf("cancelled check = %+v, want no answer", got)
	}
	if _, err := New(noGit()).Run(ctx, Request{Project: e.project, Resolved: resolvedConfig(e.cfg)}); !errors.Is(err, context.Canceled) {
		t.Errorf("doctor under a cancelled context returned %v, want context.Canceled", err)
	}
}
