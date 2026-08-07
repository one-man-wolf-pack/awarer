package gitmeta

import (
	"context"
	"fmt"
	"strings"
)

// Tracking reports whether the project sits in a git worktree and, if so, whether
// any path under .awa is tracked by git. Doctor turns AwaTracked into a warning:
// awa's durable state is local cache, not source, and committing it bloats history
// and leaks machine-specific paths.
type Tracking struct {
	InRepo     bool
	AwaTracked bool
}

// AwaTracking queries git for whether .awa is tracked. It reuses the same
// best-effort posture as Capture: a project that is not a git repo, or a host
// without git, yields InRepo=false and no error, because git's absence is normal
// and must never be reported as a project problem. A genuine git execution failure
// (not "no repo", not "no git") is returned so the caller can surface it as a
// warning. The pathspec is passed as an argument, never interpolated into a shell
// string.
func (p *Provider) AwaTracking(ctx context.Context) (Tracking, error) {
	out, stderr, err := p.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Tracking{}, ctxErr
		}
		absent, genuine := classifyProbe(err, stderr)
		if absent {
			return Tracking{InRepo: false}, nil
		}
		return Tracking{}, genuine
	}
	if strings.TrimSpace(string(out)) != "true" {
		return Tracking{InRepo: false}, nil
	}

	// ls-files lists tracked paths matching the pathspec, relative to the working
	// directory (p.root via git -C). Any output means at least one .awa path is
	// tracked. -z keeps NUL-separated paths so unusual filenames cannot confuse the
	// emptiness check.
	listed, lsStderr, err := p.run(ctx, "ls-files", "-z", "--", ".awa")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Tracking{}, ctxErr
		}
		msg := strings.TrimSpace(lsStderr)
		if msg == "" {
			msg = err.Error()
		}
		return Tracking{}, fmt.Errorf("git ls-files failed: %s", msg)
	}
	return Tracking{
		InRepo:     true,
		AwaTracked: len(strings.Trim(string(listed), "\x00")) > 0,
	}, nil
}
