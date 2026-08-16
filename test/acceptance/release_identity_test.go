package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stampAssignment is the complete linker assignment that turns a build into a release
// build. Nothing else makes a published binary report its version, and the Go linker
// accepts -X for a symbol that no longer exists without a word, so a rename anywhere along
// this string produces a green release whose binaries report a development version. The
// two tests below are what keeps that silent: one proves every writer still spells it, the
// other proves the string still reaches `awa version`.
const stampAssignment = "-X awarer/internal/app.versionStamp=awa.version="

// releaseStampWriters build a released awa and therefore have to spell the assignment the
// same way: `.goreleaser.yaml` for the archives, the Homebrew template for the formula.
// Each names what the version it substitutes looks like, because the assignment alone is
// only half the flag — one that ends right after `awa.version=` builds a binary reporting
// an empty version, which no other check here or in the renderer refuses. A third writer
// added without being listed is invisible to this guard, the one thing it cannot prove
// for itself.
var releaseStampWriters = []struct {
	path        string
	substitutes string
}{
	{"../../.goreleaser.yaml", "{{ .Tag }}"},
	{"../../packaging/homebrew/awarer.rb.tmpl", "@@AWARER_TAG@@"},
}

func TestEveryReleaseWriterCarriesTheVersionStamp(t *testing.T) {
	for _, writer := range releaseStampWriters {
		data, err := os.ReadFile(writer.path)
		if err != nil {
			t.Fatalf("reading %s: %v", writer.path, err)
		}
		want := stampAssignment + writer.substitutes
		if !strings.Contains(string(data), want) {
			t.Errorf("%s does not carry %q, so the binaries it builds would report no release version",
				filepath.Base(writer.path), want)
		}
	}
}

func TestTheVersionStampReachesAwaVersion(t *testing.T) {
	// Deliberately not a plausible release: a passing assertion has to come from this
	// build's linker flag rather than from a tag that happens to exist.
	const poison = "v9999.999.999"

	// Its own build, not the package's shared awaBin: that one carries no linker flags, so
	// asserting a version against it could only ever read the development default back.
	bin := filepath.Join(t.TempDir(), "awa")
	build := exec.Command("go", "build", "-ldflags", stampAssignment+poison, "-o", bin, "../../cmd/awa")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building awa with the release stamp: %v", err)
	}

	out, err := exec.Command(bin, "version", "--json").Output()
	if err != nil {
		t.Fatalf("running awa version: %v", err)
	}
	var envelope struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decoding awa version --json: %v", err)
	}
	if envelope.Data.Version != poison {
		t.Errorf("awa version reports %q, want %q: the assignment no longer reaches it through the named symbol and frame",
			envelope.Data.Version, poison)
	}
}
