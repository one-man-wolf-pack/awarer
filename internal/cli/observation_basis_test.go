package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"awarer/internal/app/changes"
	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/output"
)

type basisDoc struct {
	Data struct {
		Left  map[string]any `json:"left"`
		Right map[string]any `json:"right"`
		Basis *struct {
			Token      string `json:"token"`
			ScanConfig struct {
				Left      string `json:"left"`
				Right     string `json:"right"`
				Agreement string `json:"agreement"`
			} `json:"scan_config"`
		} `json:"basis"`
		PathFilters    []string       `json:"path_filters"`
		ReviewCoverage map[string]any `json:"review_coverage"`
		Scope          map[string]any `json:"scope"`
		Summary        map[string]int `json:"summary"`
		Changes        []struct {
			Status               string `json:"status"`
			OldPath              string `json:"old_path"`
			NewPath              string `json:"new_path"`
			ContentDiffAvailable bool   `json:"content_diff_available"`
		} `json:"changes"`
		Files []struct {
			Status               string `json:"status"`
			OldPath              string `json:"old_path"`
			NewPath              string `json:"new_path"`
			ContentDiffAvailable bool   `json:"content_diff_available"`
			Availability         string `json:"availability"`
		} `json:"files"`
	} `json:"data"`
}

func decodeBasis(t *testing.T, stdout, array string) basisDoc {
	t.Helper()
	var doc basisDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	b := doc.Data.Basis
	if b == nil {
		t.Fatalf("basis missing:\n%s", stdout)
	}
	if b.Token != "observation-delta" {
		t.Errorf("basis.token = %q, want observation-delta", b.Token)
	}
	if !strings.HasPrefix(b.ScanConfig.Left, "blake3:") || !strings.HasPrefix(b.ScanConfig.Right, "blake3:") {
		t.Errorf("scan_config identities = %+v, want both recorded hashes", b.ScanConfig)
	}
	if want := b.ScanConfig.Left == b.ScanConfig.Right; (b.ScanConfig.Agreement == "match") != want {
		t.Errorf("agreement = %q for left %s right %s", b.ScanConfig.Agreement, b.ScanConfig.Left, b.ScanConfig.Right)
	}
	bi := strings.Index(stdout, `"basis"`)
	ai := -1
	if loc := regexp.MustCompile(`"` + array + `":\s*\[`).FindStringIndex(stdout); loc != nil {
		ai = loc[0]
	}
	if bi < 0 || ai < 0 || bi > ai {
		t.Errorf("basis (at %d) must precede the %s array (at %d)", bi, array, ai)
	}
	if doc.Data.Left["kind"] == nil || doc.Data.Right["kind"] == nil || doc.Data.PathFilters == nil ||
		doc.Data.ReviewCoverage["token"] != "checkpoint-delta-only" || doc.Data.Scope == nil || doc.Data.Summary == nil {
		t.Errorf("existing facts missing:\n%s", stdout)
	}
	return doc
}

func mismatchWarnings(stderr string) int {
	return strings.Count(stderr, "warning: recorded scan config identities differ")
}

func writeLocalConfig(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write local config: %v", err)
	}
}

func excludeProject(t *testing.T) string {
	t.Helper()
	root := initProject(t)
	writeFile(t, root, "keep.txt", "keep\n")
	writeFile(t, root, "gen/a.txt", "a\n")
	writeFile(t, root, "gen/b.txt", "b\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	return root
}

func TestConfigExcludeChangeDisclosesMismatchInEveryMode(t *testing.T) {
	root := excludeProject(t)
	writeLocalConfig(t, root, "[scope]\nextra_excludes = [\"gen/\"]\n")
	for _, rel := range []string{"gen/a.txt", "gen/b.txt"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s must remain on disk: %v", rel, err)
		}
	}
	wantDeleted := []string{"gen", "gen/a.txt", "gen/b.txt"}

	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, stdout string)
	}{
		{"changes", []string{"changes"}, func(t *testing.T, stdout string) {
			if !strings.Contains(stdout, "basis: ") || !strings.Contains(stdout, "differs") {
				t.Errorf("header does not name a differing basis:\n%s", stdout)
			}
			for _, p := range wantDeleted {
				if !strings.Contains(stdout, "D "+p) {
					t.Errorf("stdout lacks D %s:\n%s", p, stdout)
				}
			}
		}},
		{"changes --stat", []string{"changes", "--stat"}, func(t *testing.T, stdout string) {
			if !strings.Contains(stdout, "basis: ") || !strings.Contains(stdout, "3 deleted") {
				t.Errorf("stat output:\n%s", stdout)
			}
		}},
		{"changes --name-only", []string{"changes", "--name-only"}, func(t *testing.T, stdout string) {
			lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
			slices.Sort(lines)
			if !slices.Equal(lines, wantDeleted) {
				t.Errorf("name-only stdout = %q, want only the paths %v", stdout, wantDeleted)
			}
		}},
		{"changes --json", []string{"changes", "--json"}, func(t *testing.T, stdout string) {
			doc := decodeBasis(t, stdout, "changes")
			if doc.Data.Basis.ScanConfig.Agreement != "mismatch" {
				t.Errorf("agreement = %q, want mismatch", doc.Data.Basis.ScanConfig.Agreement)
			}
			var got []string
			for _, c := range doc.Data.Changes {
				if c.Status != "deleted" {
					t.Errorf("unexpected change %+v", c)
				}
				got = append(got, c.OldPath)
			}
			slices.Sort(got)
			if !slices.Equal(got, wantDeleted) || doc.Data.Summary["deleted"] != 3 {
				t.Errorf("changes = %v summary = %v, want %v deleted", got, doc.Data.Summary, wantDeleted)
			}
		}},
		{"diff", []string{"diff"}, func(t *testing.T, stdout string) {
			if !strings.Contains(stdout, "basis: ") || !strings.Contains(stdout, "gen/a.txt") {
				t.Errorf("diff output:\n%s", stdout)
			}
		}},
		{"diff --stat", []string{"diff", "--stat"}, func(t *testing.T, stdout string) {
			if !strings.Contains(stdout, "basis: ") || !strings.Contains(stdout, "3 deleted") {
				t.Errorf("diff stat output:\n%s", stdout)
			}
		}},
		{"diff --json", []string{"diff", "--json"}, func(t *testing.T, stdout string) {
			doc := decodeBasis(t, stdout, "files")
			if doc.Data.Basis.ScanConfig.Agreement != "mismatch" || len(doc.Data.Files) != 3 {
				t.Errorf("diff json basis = %+v files = %d", doc.Data.Basis, len(doc.Data.Files))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(append(tc.args, "--root", root)...)
			if code != int(ExitSuccess) {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			if n := mismatchWarnings(stderr); n != 1 {
				t.Errorf("mismatch warnings = %d, want exactly 1; stderr = %q", n, stderr)
			}
			if strings.Contains(stdout, "warning:") {
				t.Errorf("diagnostic leaked into stdout:\n%s", stdout)
			}
			tc.check(t, stdout)
		})
	}
}

func TestIgnoreFileEditKeepsMatchingIdentity(t *testing.T) {
	root := excludeProject(t)
	writeFile(t, root, ".awaignore", "gen/\n")

	code, stdout, stderr := run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	doc := decodeBasis(t, stdout, "changes")
	if doc.Data.Basis.ScanConfig.Agreement != "match" {
		t.Errorf("agreement = %q, want match", doc.Data.Basis.ScanConfig.Agreement)
	}
	status := map[string]string{}
	for _, c := range doc.Data.Changes {
		p := c.NewPath
		if p == "" {
			p = c.OldPath
		}
		status[p] = c.Status
	}
	want := map[string]string{".awaignore": "added", "gen": "deleted", "gen/a.txt": "deleted", "gen/b.txt": "deleted"}
	if len(status) != len(want) {
		t.Errorf("changes = %v, want %v", status, want)
	}
	for p, s := range want {
		if status[p] != s {
			t.Errorf("%s = %q, want %q (all changes %v)", p, status[p], s, status)
		}
	}
	if n := mismatchWarnings(stderr); n != 0 {
		t.Errorf("matching identities must not warn; stderr = %q", stderr)
	}

	_, human, _ := run("changes", "--root", root)
	if !strings.Contains(human, "basis: ") || !strings.Contains(human, "on both sides") {
		t.Errorf("human header lacks a matching basis:\n%s", human)
	}
}

func TestTrustModeIdentityDiffersOverEqualManifests(t *testing.T) {
	root := excludeProject(t)

	code, stdout, stderr := run("changes", "--root", root, "--strict")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no changes") || !strings.Contains(stdout, "basis: ") || !strings.Contains(stdout, "differs") {
		t.Errorf("empty delta must still name its differing basis:\n%s", stdout)
	}
	if n := mismatchWarnings(stderr); n != 1 {
		t.Errorf("mismatch warnings = %d, want 1; stderr = %q", n, stderr)
	}

	code, stdout, _ = run("diff", "--root", root, "--strict", "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("diff --json exit = %d", code)
	}
	doc := decodeBasis(t, stdout, "files")
	if doc.Data.Basis.ScanConfig.Agreement != "mismatch" || len(doc.Data.Files) != 0 {
		t.Errorf("basis = %+v files = %d, want mismatch over an empty delta", doc.Data.Basis, len(doc.Data.Files))
	}
}

func TestRetentionOnlyChangeKeepsIdentityAndByteAvailability(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, ".env", "DUMMY_TOKEN=one\n")
	writeFile(t, root, "a.txt", "one\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	writeLocalConfig(t, root, "[checkpoint]\nhash_only_patterns = []\n")
	writeFile(t, root, ".env", "DUMMY_TOKEN=second\n")
	writeFile(t, root, "a.txt", "second\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := run("diff", "@-2..@-1", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("diff exit = %d, stderr = %q", code, stderr)
	}
	doc := decodeBasis(t, stdout, "files")
	if doc.Data.Basis.ScanConfig.Agreement != "match" || mismatchWarnings(stderr) != 0 {
		t.Errorf("retention-only change must keep matching identities: %+v, stderr %q", doc.Data.Basis, stderr)
	}
	avail := map[string]bool{}
	for _, f := range doc.Data.Files {
		if f.Status != "modified" {
			t.Errorf("unexpected file %+v", f)
		}
		avail[f.NewPath] = f.ContentDiffAvailable
	}
	if len(avail) != 2 || avail[".env"] || !avail["a.txt"] {
		t.Errorf("content_diff_available = %v, want .env false and a.txt true", avail)
	}
}

func TestEmptyPathFilteredDeltaCarriesBasis(t *testing.T) {
	root := excludeProject(t)
	writeFile(t, root, "keep.txt", "changed\n")

	code, stdout, stderr := run("changes", "--root", root, "--json", "--", filepath.Join(root, "gen"))
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	doc := decodeBasis(t, stdout, "changes")
	if doc.Data.Basis.ScanConfig.Agreement != "match" || len(doc.Data.Changes) != 0 || doc.Data.Summary["deleted"] != 0 || doc.Data.Summary["modified"] != 0 {
		t.Errorf("filtered delta = %+v, want an empty matching delta", doc.Data)
	}
	if !slices.Equal(doc.Data.PathFilters, []string{"gen"}) {
		t.Errorf("path_filters = %v, want [gen]", doc.Data.PathFilters)
	}
}

func mismatchedStates(t *testing.T) (*state.ResolvedState, *state.ResolvedState) {
	t.Helper()
	side := func(digit string) *state.ResolvedState {
		h, err := hashing.ParseConfigHash("blake3:" + strings.Repeat(digit, 64))
		if err != nil {
			t.Fatalf("ParseConfigHash: %v", err)
		}
		return &state.ResolvedState{Kind: state.KindCheckpoint, Header: &checkpoint.CheckpointHeader{ScanConfigHash: h}}
	}
	return side("a"), side("b")
}

func TestMismatchAdvisoryNeverMasksAPartialStream(t *testing.T) {
	left, right := mismatchedStates(t)
	sentinel := errors.New("manifest line 2 is corrupt")

	t.Run("human", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		sr := changes.StreamResult{Left: left, Right: right, Changes: &oneThenErrCursor{change: addedChange(t, "a.txt"), err: sentinel}}
		err := renderChanges(output.New(&stdout, &stderr), sr, nil, false, false, domainconfig.DefaultTimeDisplay(), baselineGitContext{})
		if err == nil || !strings.Contains(err.Error(), "partial output:") {
			t.Fatalf("err = %v, want partial output", err)
		}
		if n := mismatchWarnings(stderr.String()); n != 1 {
			t.Errorf("mismatch warnings = %d, want 1; stderr = %q", n, stderr.String())
		}
		if strings.Contains(stdout.String(), "no changes") {
			t.Errorf("a failed stream must not look complete:\n%s", stdout.String())
		}
	})
	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		sr := changes.StreamResult{Left: left, Right: right, Changes: &oneThenErrCursor{change: addedChange(t, "a.txt"), err: sentinel}}
		err := streamChangesJSON(output.New(&stdout, &stderr), sr, nil, domainconfig.Defaults(), baselineGitContext{})
		if err == nil || !strings.Contains(err.Error(), "partial output:") {
			t.Fatalf("err = %v, want partial output", err)
		}
		if mismatchWarnings(stderr.String()) != 1 {
			t.Errorf("stderr = %q, want one mismatch warning", stderr.String())
		}
		out := stdout.String()
		if json.Valid([]byte(out)) {
			t.Errorf("a failed stream produced a parseable document:\n%s", out)
		}
		if bi, ci := strings.Index(out, `"basis"`), strings.Index(out, `"a.txt"`); bi < 0 || ci < 0 || bi > ci {
			t.Errorf("basis must precede streamed elements:\n%s", out)
		}
	})
}
