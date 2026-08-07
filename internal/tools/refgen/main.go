// Command refgen prints, atomically writes, or checks awa's deterministic
// machine-readable CLI reference. It is a build/dev tool, not a shipped binary:
// it lives under internal/ so it is never part of the public `awa` surface, and it
// exists so the docs pipeline has one checked reference input to derive or verify
// against.
//
// The reference is byte-identical across runs and reads no project, environment,
// git, clock, or filesystem state. A generation error exits non-zero and writes
// no partial output.
//
// Usage:
//
//	go run ./internal/tools/refgen                        # write to stdout
//	go run ./internal/tools/refgen -output <file>         # atomically replace <file> (`just reference-update`)
//	go run ./internal/tools/refgen -check -golden <file>  # prove determinism and match <file>
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"awarer/internal/cli"
)

func main() {
	output := flag.String("output", "", "atomically write the reference to this file instead of stdout")
	check := flag.Bool("check", false, "prove the generator is deterministic and matches -golden; write nothing")
	golden := flag.String("golden", "", "the committed reference the -check mode compares against")
	flag.Parse()

	// Both paths this tool takes come from a recipe as `-flag "$SOME_PATH"`, and an
	// unquoted expansion of a path holding a space would arrive as a flag value plus a
	// leftover word. Ignoring that word would mean writing or checking a file nobody
	// named, so it is refused — the same guard sitegen already carries.
	if flag.NArg() > 0 {
		fail(fmt.Errorf("unexpected argument %q", flag.Arg(0)))
	}

	if *check {
		if *output != "" {
			fail(fmt.Errorf("-check writes nothing; drop -output"))
		}
		if *golden == "" {
			fail(fmt.Errorf("-check needs -golden"))
		}
		if err := runCheck(*golden); err != nil {
			fail(err)
		}
		return
	}
	if *golden != "" {
		fail(fmt.Errorf("-golden is only meaningful with -check"))
	}

	// Build and validate the whole document in memory first, so neither mode writes
	// a partial reference if generation fails.
	data, err := generate()
	if err != nil {
		fail(err)
	}

	if *output == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			fail(err)
		}
		return
	}
	if err := atomicWrite(*output, data); err != nil {
		fail(err)
	}
}

// generate renders one complete reference document into memory.
func generate() ([]byte, error) {
	var buf bytes.Buffer
	if err := cli.WriteReferenceJSON(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runCheck is the whole reference gate: generate twice, require the two runs to
// agree, then require the result to equal the committed golden.
//
// Generating twice is not ceremony. Go randomizes map iteration on every range, so
// a reference that reached the document through an unsorted map differs between two
// generations in one process exactly as it did between the two processes the old
// shell recipe spawned — and doing it here removes the temp files, the trap, and the
// two external diffs that protocol needed.
func runCheck(goldenPath string) error {
	first, err := generate()
	if err != nil {
		return err
	}
	second, err := generate()
	if err != nil {
		return err
	}
	if err := requireIdentical("generator output is not deterministic", first, second); err != nil {
		return err
	}

	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		return err
	}
	if err := requireIdentical(
		fmt.Sprintf("CLI reference drifted from %s; regenerate with: just reference-update", goldenPath),
		golden, first,
	); err != nil {
		return err
	}
	fmt.Printf("reference-check OK: %d bytes, identical across two generations and equal to %s\n",
		len(first), goldenPath)
	return nil
}

// requireIdentical compares two documents and, when they differ, reports where. An
// empty document is refused before the comparison: two empty results are equal, and
// a gate that passes because it compared nothing is the failure this whole lane
// exists to prevent.
func requireIdentical(subject string, want, got []byte) error {
	if len(want) == 0 || len(got) == 0 {
		return fmt.Errorf("%s: refusing to compare an empty reference", subject)
	}
	if bytes.Equal(want, got) {
		return nil
	}
	return fmt.Errorf("%s\n%s", subject, describeDifference(want, got))
}

// describeDifference names the first differing line and shows both versions of it,
// which is what a reader needs and is far less output than a full diff of a large
// generated document.
func describeDifference(want, got []byte) string {
	wantLines := bytes.Split(want, []byte("\n"))
	gotLines := bytes.Split(got, []byte("\n"))
	for i := range max(len(wantLines), len(gotLines)) {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if bytes.Equal(w, g) {
			continue
		}
		return fmt.Sprintf("first difference at line %d:\n  committed: %s\n  generated: %s", i+1, w, g)
	}
	return fmt.Sprintf("the documents differ in length only (%d vs %d bytes)", len(want), len(got))
}

func lineAt(lines [][]byte, i int) []byte {
	if i >= len(lines) {
		return []byte("<end of file>")
	}
	return lines[i]
}

// atomicWrite publishes data to path atomically: it writes a temp file in the same
// directory, gives it the existing file's mode (or 0644 for a new file), then
// renames it over path. A failure at any step removes the temp and leaves an
// existing target untouched. Keeping this in Go avoids a shell/python dependency
// and the mktemp-mode leak a shell updater caused.
func atomicWrite(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".refgen-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if err := writeTemp(tmp, data, mode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeTemp writes data to an open temp file, sets its mode, and closes it. It owns
// closing f so the caller can remove the temp on any returned error.
func writeTemp(f *os.File, data []byte, mode os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "refgen: %v\n", err)
	os.Exit(1)
}
