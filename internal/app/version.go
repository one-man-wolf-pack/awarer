// Package app holds awa's use-case level command handlers. Handlers receive
// their dependencies explicitly (output streams, parsed options) and contain no
// CLI parsing and no product behavior beyond the use case they own.
package app

import (
	"runtime/debug"
	"strings"

	"awarer/internal/output"
)

// versionFramePrefix frames the linker-injected build stamp. Two files write the matching
// flag — `.goreleaser.yaml` for the release archives and `packaging/homebrew/awarer.rb.tmpl`
// for the source-built Homebrew formula. Changing this without changing both leaves the
// binaries built by the one that was missed reporting the raw stamp in place of a version.
const versionFramePrefix = "awa.version="

// versionStamp is the framed build stamp, injected with one linker flag — the second
// `=` belongs to the frame:
//
//	go build -ldflags "-X awarer/internal/app.versionStamp=awa.version=v1.2.3" ./cmd/awa
//
// The version is the release tag verbatim, leading "v" included. It defaults to a
// development marker.
var versionStamp = versionFramePrefix + "0.0.0-dev"

// version is the awa release version, parsed from the framed stamp so there is a single
// injection point and the stamp and the version can never disagree.
var version = strings.TrimPrefix(versionStamp, versionFramePrefix)

// VersionString returns the awa release version embedded in checkpoints and other
// records. It is the version field of the resolved build info, exposed so use
// cases can stamp it without reaching into unexported state.
func VersionString() string { return readBuildInfo().Version }

// SourceCommit returns the source commit the exported documentation may claim as
// its origin, or "" when there is none to claim. It exists for the documentation
// export's provenance, which records stable build identity only: deliberately not
// the build time or the dirty flag, which would make two exports of the same
// binary differ or leak the machine that built it.
func SourceCommit() string { return sourceCommit(readBuildInfo()) }

// sourceCommit is the publishable-commit decision, split out so it can be tested
// without building a binary in each VCS state. A commit is published only when it
// actually describes the exported content: a binary built from a modified
// worktree carries the revision it branched from, not the source it was compiled
// from, so publishing it would make the manifest assert something false. The
// export stays authoritative through its own document hashes; the dirty flag
// itself is never published, because mutable build provenance must not ship.
func sourceCommit(bi BuildInfo) string {
	if bi.modified {
		return ""
	}
	return bi.Revision
}

// BuildInfo is the resolved version and build metadata for the running binary.
//
// modified records whether the toolchain saw uncommitted changes when it built
// the binary. It is unexported on purpose: it decides whether a commit may be
// published as provenance, but it is never itself an output field.
type BuildInfo struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
	Go       string `json:"go"`

	modified bool
}

// readBuildInfo assembles BuildInfo from the ldflags-injected version and the
// VCS stamping that the Go toolchain embeds via runtime/debug. Missing fields
// are left empty rather than guessed.
func readBuildInfo() BuildInfo {
	bi := BuildInfo{Version: version}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return bi
	}
	bi.Go = info.GoVersion
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			bi.Revision = s.Value
		case "vcs.time":
			bi.Time = s.Value
		case "vcs.modified":
			bi.modified = s.Value == "true"
		}
	}
	return bi
}

// human renders BuildInfo as a single concise line, e.g.
// "awa 0.0.0-dev (abcd123, go1.26.5)".
func (b BuildInfo) human() string {
	s := "awa " + b.Version
	var extra []string
	if b.Revision != "" {
		rev := b.Revision
		if len(rev) > 7 {
			rev = rev[:7]
		}
		extra = append(extra, rev)
	}
	if b.Go != "" {
		extra = append(extra, b.Go)
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}

// Version writes the binary's version and build metadata. With JSON enabled it
// emits the schema-versioned envelope; otherwise it writes the human line.
func Version(w *output.Writer, jsonOut bool) error {
	bi := readBuildInfo()
	if jsonOut {
		return w.JSON("version", bi)
	}
	w.Line(bi.human())
	return nil
}
