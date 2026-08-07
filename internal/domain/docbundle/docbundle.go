// Package docbundle is the closed value model for one exported documentation
// bundle: the ordered documents a released awa binary carries, the generated
// machine reference beside them, and the versioned manifest that describes both.
//
// It exists so "a complete, safe, deterministic bundle" is a type rather than a
// convention. Slugs and output paths are constructor-validated value objects, so
// after construction a traversing path, a platform separator, an empty name, or
// a duplicate output target is unrepresentable — the exporter cannot publish one
// by accident, and no downstream consumer has to re-validate. A Bundle owns its
// bytes defensively (copied in, copied out) and renders its manifest
// deterministically: two renderings of the same bundle are byte-identical.
//
// The package is pure. It performs no I/O and reads no clock, environment,
// working directory, or filesystem state, so nothing ambient can reach the
// published output.
package docbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ExportSchemaVersion is the version of the emitted manifest shape. It is
// deliberately independent of the CLI JSON envelope version and of the machine
// reference's own schema version: the export is a separate contract with its own
// consumers. Before v1 a breaking change replaces the shape rather than
// accumulating legacy readers; after v1 an incompatible change bumps this.
const ExportSchemaVersion = 1

// ManifestName is the fixed manifest filename at the bundle root. It is the
// last file an exporter publishes, so its presence means the bundle is complete.
const ManifestName = "manifest.json"

// Kind is the closed set of document kinds a bundle can carry. It is closed on
// purpose rather than an extensible string bag: a consumer that switches on kind
// must handle every case the export can actually produce.
type Kind string

const (
	// KindTopic is an authored operational page.
	KindTopic Kind = "topic"
	// KindCommand is a generated page describing a command's syntax surface.
	KindCommand Kind = "command"
	// KindReference is a generated page rendered from a live model.
	KindReference Kind = "reference"
)

// valid reports whether k is one of the closed kinds.
func (k Kind) valid() bool {
	switch k {
	case KindTopic, KindCommand, KindReference:
		return true
	default:
		return false
	}
}

// ParseKind turns a raw kind token into a Kind, so a catalog declared in plain
// strings enters the closed set at one checked point instead of casting.
func ParseKind(raw string) (Kind, error) {
	k := Kind(raw)
	if !k.valid() {
		return "", fmt.Errorf("docbundle: %q is not a document kind", raw)
	}
	return k, nil
}

// slugRe is the stable slug spelling: lowercase alphanumerics separated by
// single hyphens. A slug in this shape is usable unchanged as a filename, a URL
// path segment, and a manifest key, so no surface has to escape it.
var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Slug is a validated stable document identifier.
type Slug struct{ s string }

// NewSlug validates and constructs a Slug. It is the only way to make one, so a
// Slug value is always publishable as-is.
func NewSlug(raw string) (Slug, error) {
	if !slugRe.MatchString(raw) {
		return Slug{}, fmt.Errorf("docbundle: %q is not a valid slug", raw)
	}
	return Slug{s: raw}, nil
}

// String returns the slug text.
func (s Slug) String() string { return s.s }

// DocPath is a validated bundle-relative output path. Construction rejects
// anything that could escape the bundle root, address a platform-specific path,
// or resolve to a different file than it reads as.
type DocPath struct{ p string }

// NewDocPath validates and constructs a DocPath: relative, slash separated,
// already clean, with no empty, "." or ".." component, and a non-empty basename
// carrying an extension.
func NewDocPath(raw string) (DocPath, error) {
	switch {
	case raw == "":
		return DocPath{}, fmt.Errorf("docbundle: empty output path")
	case strings.HasPrefix(raw, "/"):
		return DocPath{}, fmt.Errorf("docbundle: output path %q must be relative", raw)
	case strings.Contains(raw, `\`):
		return DocPath{}, fmt.Errorf("docbundle: output path %q must use slash separators", raw)
	case path.Clean(raw) != raw:
		return DocPath{}, fmt.Errorf("docbundle: output path %q is not clean", raw)
	}
	for _, c := range strings.Split(raw, "/") {
		if c == "" || c == "." || c == ".." {
			return DocPath{}, fmt.Errorf("docbundle: output path %q has an unsafe component", raw)
		}
	}
	if path.Ext(path.Base(raw)) == "" {
		return DocPath{}, fmt.Errorf("docbundle: output path %q has no file extension", raw)
	}
	return DocPath{p: raw}, nil
}

// String returns the bundle-relative path with slash separators.
func (p DocPath) String() string { return p.p }

// Dir returns the bundle-relative directory holding the file, or "" when the
// file sits at the bundle root. It is what a publisher needs to create parents.
func (p DocPath) Dir() string {
	i := strings.LastIndex(p.p, "/")
	if i < 0 {
		return ""
	}
	return p.p[:i]
}

// Base returns the final path component.
func (p DocPath) Base() string {
	i := strings.LastIndex(p.p, "/")
	if i < 0 {
		return p.p
	}
	return p.p[i+1:]
}

// sum returns the lowercase hex SHA-256 of data. Content hashes let a consumer
// verify a published file against the manifest without re-deriving it.
func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
