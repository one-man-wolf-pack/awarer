package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/app/docsexport"
)

// awa docs export must work with no project on disk: every test here uses the
// bare run(...) helper from a directory that has no .awa/, proving the export
// never discovers a root, loads config, or takes a lock.

func TestDocsExportPublishesTheBundle(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	code, stdout, stderr := run("docs", "export", "--output", dest)
	if code != int(ExitSuccess) {
		t.Fatalf("docs export exit = %d, want %d; stderr=%q", code, ExitSuccess, stderr)
	}
	if stderr != "" {
		t.Errorf("docs export stderr = %q, want empty", stderr)
	}

	manifestPath := filepath.Join(dest, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	var m struct {
		Documents []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"documents"`
		MachineReference struct {
			Path string `json:"path"`
		} `json:"machine_reference"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	for _, d := range m.Documents {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(d.Path))); err != nil {
			t.Errorf("manifest lists %q but it was not published: %v", d.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(m.MachineReference.Path))); err != nil {
		t.Errorf("machine reference %q was not published: %v", m.MachineReference.Path, err)
	}

	// The summary is one compact human line naming what a reviewer needs: how
	// much was published, under which schema and version, and where.
	if !strings.Contains(stdout, "exported ") || !strings.Contains(stdout, dest) {
		t.Errorf("docs export summary is not informative:\n%s", stdout)
	}
	if !strings.Contains(stdout, manifestPath) {
		t.Errorf("docs export summary omits the manifest path:\n%s", stdout)
	}
}

// TestDocsExportIsByteDeterministic is the end-to-end form of the determinism
// guarantee: two exports of one binary produce the same paths and the same bytes.
func TestDocsExportIsByteDeterministic(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dest := range []string{a, b} {
		if code, _, stderr := run("docs", "export", "--output", dest); code != int(ExitSuccess) {
			t.Fatalf("docs export into %s: exit %d, stderr=%q", dest, code, stderr)
		}
	}

	aFiles, bFiles := exportedTree(t, a), exportedTree(t, b)
	if len(aFiles) != len(bFiles) {
		t.Fatalf("exports differ in file count: %d vs %d", len(aFiles), len(bFiles))
	}
	for path, content := range aFiles {
		other, ok := bFiles[path]
		if !ok {
			t.Errorf("second export is missing %q", path)
			continue
		}
		if content != other {
			t.Errorf("exported %q differs between two runs of the same binary", path)
		}
	}
}

// exportedTree reads an export into a slash-relative path -> content map.
func exportedTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// TestExportedDocumentsCarryNoAmbientState proves the export is a pure function
// of the binary: no absolute path, working directory, user name, hostname, or
// environment value from the exporting machine may appear in a published
// document. Anything ambient would make two machines produce different bundles.
func TestExportedDocumentsCarryNoAmbientState(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	if code, _, stderr := run("docs", "export", "--output", dest); code != int(ExitSuccess) {
		t.Fatalf("docs export: exit %d, stderr=%q", code, stderr)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	forbidden := []string{dest, wd, os.TempDir()}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		forbidden = append(forbidden, home)
	}

	for path, body := range exportedTree(t, dest) {
		for _, needle := range forbidden {
			if needle == "" || needle == "/" {
				continue
			}
			if strings.Contains(body, needle) {
				t.Errorf("exported %q leaks the exporting machine's path %q", path, needle)
			}
		}
	}
}

func TestDocsExportUsageErrors(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{"docs"}, "docs requires a subcommand"},
		{"unknown subcommand", []string{"docs", "publish"}, "unknown docs subcommand"},
		{"missing --output", []string{"docs", "export"}, "requires --output"},
		{"empty --output", []string{"docs", "export", "--output", ""}, "requires a non-empty value"},
		{"unknown flag", []string{"docs", "export", "--output", dest, "--not-a-real-flag"}, "unknown flag"},
		{"stray operand", []string{"docs", "export", "extra", "--output", dest}, "unexpected argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := run(c.args...)
			if code != int(ExitUsageError) {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsageError, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, c.want)
			}
		})
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a rejected invocation created the destination: %v", err)
	}
}

// TestDocsExportRejectsGlobalOptions proves the command declares no capabilities:
// an option that would imply project or config work is a loud usage error rather
// than an accepted no-op, which is what keeps the export pure.
func TestDocsExportRejectsGlobalOptions(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	for _, global := range [][]string{
		{"--json"},
		{"--root", t.TempDir()},
		{"--config", "awa.toml"},
		{"--strict"},
	} {
		args := append(append([]string{}, global...), "docs", "export", "--output", dest)
		code, _, stderr := run(args...)
		if code != int(ExitUsageError) {
			t.Errorf("awa %s: exit = %d, want %d; stderr=%q", strings.Join(args, " "), code, ExitUsageError, stderr)
		}
	}
}

// TestDocsExportFailureMapping pins which failures keep their message. The exit
// codes are the easy half; the half that has to be locked down is that a
// cancellation which already reserved the destination is NOT reported with the
// standard terse "interrupted", because that directory is still on disk and the
// message is the only thing that says so.
//
// The inputs are built from the exported error identities alone — the contract
// between the two packages — and carry a deliberately synthetic message, so this
// asserts the mapping rather than re-stating the exporter's prose.
func TestDocsExportFailureMapping(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		wantCode ExitCode
		wantMsg  string
	}{
		{
			"a refused destination is the user's argument",
			fmt.Errorf("marker: %w", docsexport.ErrUnsafeDestination),
			ExitUsageError, "marker",
		},
		{
			"a cancellation that left a directory keeps its instruction",
			fmt.Errorf("marker: %w, %w", context.Canceled, docsexport.ErrIncompleteExport),
			ExitInterrupted, "marker",
		},
		{
			"a cancellation that created nothing stays terse",
			fmt.Errorf("marker: %w", context.Canceled),
			ExitInterrupted, "interrupted",
		},
		{
			"an ordinary failure that left a directory keeps its instruction",
			fmt.Errorf("marker: %w", docsexport.ErrIncompleteExport),
			ExitGenericError, "marker",
		},
		{
			"an ordinary failure",
			errors.New("marker"),
			ExitGenericError, "marker",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := docsExportFailure(c.err)
			if got.Code() != c.wantCode {
				t.Errorf("code = %d, want %d", got.Code(), c.wantCode)
			}
			if !strings.Contains(got.Error(), c.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", got.Error(), c.wantMsg)
			}
		})
	}

	// The terse case is the one that could pass by accident: a mapping that always
	// kept the cause would satisfy every "contains marker" assertion above.
	if msg := docsExportFailure(fmt.Errorf("marker: %w", context.Canceled)).Error(); strings.Contains(msg, "marker") {
		t.Errorf("a cancellation that created nothing reported %q, want no cause detail", msg)
	}
}

// TestDocsExportRefusesUnsafeDestination proves the CLI maps a refused
// destination to a usage error (exit 2) and publishes nothing — the user's
// existing files are untouched and no manifest is left behind.
func TestDocsExportRefusesUnsafeDestination(t *testing.T) {
	dest := t.TempDir()
	keep := filepath.Join(dest, "keep.txt")
	if err := os.WriteFile(keep, []byte("user data\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	code, _, stderr := run("docs", "export", "--output", dest)
	if code != int(ExitUsageError) {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsageError, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want it to explain the destination already exists", stderr)
	}
	if body, err := os.ReadFile(keep); err != nil || string(body) != "user data\n" {
		t.Errorf("the user's file was disturbed: %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); !os.IsNotExist(err) {
		t.Errorf("a refused export left a manifest: %v", err)
	}
}
