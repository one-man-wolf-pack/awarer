package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"time"

	"awarer/internal/app"
	"awarer/internal/app/checkpointcmd"
	"awarer/internal/app/configcmd"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/output"
)

// buildCheckpointService wires the composition root for "awa checkpoint": the
// scanner (with its worktree index for hash reuse), the blob store, the checkpoint
// repository, and the git provider. It returns a cleanup that closes the index,
// which the caller must defer. checkpoint is the first real consumer of the
// scanner, so this is the one place that assembles it.
//
// The direct content reader is anchored at the same root the walker walks, so an
// ordinary entry reopened from its recorded path at materialization time travels the
// identical verified traversal the walk's own opener would have.
func buildCheckpointService(ctx context.Context, layout paths.Layout, cfg domainconfig.Config) (*checkpointcmd.Service, func(), error) {
	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		return nil, nil, stateActionErrorf("opening worktree index: %v", err)
	}
	svc := checkpointcmd.New(checkpointcmd.Deps{
		Scanner:     scanner.New(worktreefs.New(), hasher, idx),
		Index:       idx,
		Hasher:      hasher,
		Blobs:       blobstore.New(layout, hasher),
		Checkpoints: checkpointjson.NewRepo(layout),
		Git:         gitmeta.New(layout.Root()),
		Content:     worktreefs.NewContentReader(layout.Root()),
		Now:         time.Now,
		Rand:        rand.Reader,
		Version:     app.VersionString(),
		Locks:       newPresenceLocker(ctx, layout, lockfile.OpCheckpoint, cfg.Locks.Timeout.Duration()),
	})
	return svc, func() { _ = idx.Close() }, nil
}

// runCheckpoint handles "awa checkpoint". It checkpoints the whole configured
// scope; it takes no path arguments, because scope is configuration, not a
// per-invocation choice.
func runCheckpoint(w *output.Writer, inv invocation) error {
	var message checkpoint.CheckpointMessage
	args := inv.args
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--message", "-m":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			m, err := checkpoint.ParseCheckpointMessage(v)
			if err != nil {
				return usageErrorf("%v", err)
			}
			message = m
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for checkpoint", tok)
			}
			return usageErrorf("awa checkpoint takes no path arguments (got %q)\nawa checkpoints the whole configured scope; set scope in config, not on the command line", tok)
		}
	}

	proj, err := resolveProject(inv.options)
	if err != nil {
		return err
	}
	// checkpoint writes durable state into .awa/; refuse to grow it in a directory git
	// could pick up. Restoration is silent and idempotent when the guard is merely
	// missing.
	if err := guardPreflight(w, proj, true); err != nil {
		return err
	}
	layout, err := proj.Paths()
	if err != nil {
		return err
	}
	// The project supplies the storage location; the shared awa.toml and local
	// .awa/config.toml layer under any --config override. Honoring the layers
	// matters: they can change scope, trust mode, or store_file_contents, so
	// silently ignoring one would checkpoint under a config the user did not ask for.
	files, err := projectLayerFiles(layout, inv.options)
	if err != nil {
		return err
	}
	loaded, err := configcmd.LoadLayered(files, trustModeOverride(inv.options))
	if err != nil {
		return classifyConfigLoad(err)
	}
	cfg := loaded.Config()

	svc, cleanup, err := buildCheckpointService(inv.invCtx(), layout, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	cwd, err := os.Getwd()
	if err != nil {
		return genericErrorf("determining current directory: %v", err)
	}

	res, err := svc.Run(inv.invCtx(), checkpointcmd.Request{
		Project:    proj,
		Config:     cfg,
		CommandCwd: cwd,
		Message:    message,
	})
	if err != nil {
		// A collector that outlasts the lock timeout is exit 6; an unknown conflicting
		// lock is generic. On-disk state corruption — a corrupt stored blob or a
		// corrupt checkpoint record — is exit 5 (needs repair). A hash mismatch (the
		// worktree changed under the scan) and any other failure are ordinary generic
		// errors: the store is intact, the live tree moved.
		if c := interruptError(err); c != nil {
			return c
		}
		if coded := lockErrorCode(err); coded != nil {
			return coded
		}
		if errors.Is(err, blob.ErrCorruptBlob) || errors.Is(err, checkpoint.ErrCorruptStore) {
			return stateActionErrorf("%v", err)
		}
		return genericErrorf("%v", err)
	}

	if res.GitWarning != "" {
		w.Diagnostic("warning: " + res.GitWarning)
	}

	if inv.options.JSON {
		return w.JSON("checkpoint", checkpointViewOf(res))
	}
	renderCheckpoint(w, res)
	return nil
}

type checkpointView struct {
	ID                   string                     `json:"id"`
	ShortID              string                     `json:"short_id"`
	CreatedAt            string                     `json:"created_at"`
	Message              string                     `json:"message,omitempty"`
	TreeHash             string                     `json:"tree_hash"`
	ScanConfigHash       string                     `json:"scan_config_hash"`
	CheckpointPolicyHash string                     `json:"checkpoint_policy_hash"`
	TrustMode            string                     `json:"trust_mode"`
	Stats                checkpoint.CheckpointStats `json:"stats"`
	Blobs                checkpointBlobs            `json:"blobs"`
	Git                  *checkpointGit             `json:"git"`
}

type checkpointBlobs struct {
	Written int `json:"written"`
	Reused  int `json:"reused"`
}

type checkpointGit struct {
	Branch      string `json:"branch,omitempty"`
	ShortCommit string `json:"short_commit,omitempty"`
	Clean       bool   `json:"clean"`
}

func checkpointViewOf(res checkpointcmd.Result) checkpointView {
	s := res.Header
	v := checkpointView{
		ID:                   s.ID.String(),
		ShortID:              s.ID.Short(),
		CreatedAt:            s.CreatedAt.UTC().Format(time.RFC3339Nano),
		Message:              s.Message.String(),
		TreeHash:             s.TreeHash.String(),
		ScanConfigHash:       s.ScanConfigHash.String(),
		CheckpointPolicyHash: s.CheckpointPolicyHash.String(),
		TrustMode:            s.TrustMode.String(),
		Stats:                s.Stats,
		Blobs:                checkpointBlobs{Written: res.BlobsWritten, Reused: res.BlobsReused},
	}
	if s.Git != nil {
		v.Git = &checkpointGit{Branch: s.Git.Branch, ShortCommit: s.Git.ShortCommit, Clean: s.Git.Dirty.Clean}
	}
	return v
}

func renderCheckpoint(w *output.Writer, res checkpointcmd.Result) {
	s := res.Header
	w.Linef("created checkpoint %s", s.ID.Short())
	if !s.Message.IsZero() {
		w.Linef("message:    %s", s.Message.FirstLine())
	}
	// The full id and explicit follow-up ranges make this checkpoint reusable across
	// a long review/fix loop: latest is shared project-local state and may move if
	// another agent checkpoints, so a reviewer who keeps this id can always compare
	// against exactly this checkpoint with <id>..now rather than the moving latest.
	id := s.ID.String()
	w.Linef("id:         %s", id)
	w.Linef("compare:    awa changes %s..now", id)
	w.Linef("diff:       awa diff %s..now", id)
	w.Linef("files:      %d (%d blobs, %d hash-only)", s.Stats.Files, s.Stats.Blobs, s.Stats.HashOnly)
	if s.Stats.Skipped > 0 {
		w.Linef("skipped:    %d", s.Stats.Skipped)
	}
	w.Linef("blobs:      %d new, %d reused", res.BlobsWritten, res.BlobsReused)
	if s.Git != nil {
		w.Linef("git:        %s", formatGitSummary(s.Git))
	}
}

// formatGitSummary renders a checkpoint's stored git context in the canonical
// "git:" grammar. A stored checkpoint carries no commit subject, so that part is
// omitted; the branch, short commit, and dirty state are always present.
func formatGitSummary(g *checkpoint.GitMetadata) string {
	return formatGitLine(g.Branch, g.ShortCommit, "", worktreeState(g.Dirty.Clean))
}

// formatGitLine is the single formatter for the "git:" label's value across every
// surface (checkpoint, log, status, changes/diff), so the same label always reads with
// the same grammar. branch names the branch ("branch <name> @ <short>") or, when empty
// (detached or unknown), anchors on "HEAD <short>". subject and state are optional and
// omitted when empty, because not every surface knows both: a stored checkpoint has no
// subject, and the changes/diff header names HEAD identity without a worktree state.
func formatGitLine(branch, shortCommit, subject, state string) string {
	var b strings.Builder
	if branch != "" {
		b.WriteString("branch " + branch + " @ " + shortCommit)
	} else {
		b.WriteString("HEAD " + shortCommit)
	}
	if subject != "" {
		b.WriteString(" \"" + subject + "\"")
	}
	if state != "" {
		b.WriteString(" (" + state + ")")
	}
	return b.String()
}

// worktreeState renders a worktree's clean/dirty state as the token the "git:" line's
// trailing "(state)" carries. It is distinct from the git availability tokens
// (gitStateAvailable/…), which classify whether git could be read at all.
func worktreeState(clean bool) string {
	if clean {
		return "clean"
	}
	return "dirty"
}
