package worktree_test

import (
	"bytes"
	"testing"

	"awarer/internal/domain/worktree"
)

func TestParseRelPathNormalizes(t *testing.T) {
	// Backslashes are intentionally not normalized: on Unix a backslash is a
	// legal filename character, and the walker hands ParseRelPath OS-native paths
	// already converted via filepath.ToSlash.
	cases := map[string]string{
		"a/b.go":      "a/b.go",
		"./a/b.go":    "a/b.go",
		"a/./b.go":    "a/b.go",
		"a/c/../b.go": "a/b.go",
	}
	for in, want := range cases {
		p, err := worktree.ParseRelPath(in)
		if err != nil {
			t.Errorf("ParseRelPath(%q) error: %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("ParseRelPath(%q) = %q, want %q", in, p.String(), want)
		}
	}
}

func TestParseRelPathRejects(t *testing.T) {
	bad := []string{"", ".", "/abs/path", "..", "../escape", "a/../.."}
	for _, in := range bad {
		if _, err := worktree.ParseRelPath(in); err == nil {
			t.Errorf("ParseRelPath(%q) = nil error, want rejection", in)
		}
	}
}

func TestRelPathLess(t *testing.T) {
	a, _ := worktree.ParseRelPath("a.go")
	b, _ := worktree.ParseRelPath("b.go")
	if !a.Less(b) || b.Less(a) {
		t.Errorf("RelPath.Less ordering wrong")
	}
}

func TestFileKindRoundTrip(t *testing.T) {
	for _, k := range []worktree.FileKind{worktree.KindRegular, worktree.KindDir, worktree.KindSymlink, worktree.KindSpecial} {
		got, err := worktree.ParseFileKind(k.String())
		if err != nil {
			t.Errorf("ParseFileKind(%q): %v", k.String(), err)
		}
		if got != k {
			t.Errorf("round trip %v -> %q -> %v", k, k.String(), got)
		}
		if !k.Valid() {
			t.Errorf("%v reported invalid", k)
		}
	}
}

func TestStatSignatureEqualNormal(t *testing.T) {
	base := worktree.StatSignature{Size: 10, MtimeNs: 100, CtimeNs: 200, Mode: 0o644, Dev: 1, Ino: 2, Nlink: 1}

	same := base
	if !base.EqualNormal(same) {
		t.Errorf("identical signatures should be equal under normal")
	}

	// Each field difference must break equality.
	mutations := []func(*worktree.StatSignature){
		func(s *worktree.StatSignature) { s.Size = 11 },
		func(s *worktree.StatSignature) { s.MtimeNs = 101 },
		func(s *worktree.StatSignature) { s.CtimeNs = 201 },
		func(s *worktree.StatSignature) { s.Mode = 0o600 },
		func(s *worktree.StatSignature) { s.Dev = 9 },
		func(s *worktree.StatSignature) { s.Ino = 9 },
		func(s *worktree.StatSignature) { s.Nlink = 9 },
	}
	for i, mut := range mutations {
		other := base
		mut(&other)
		if base.EqualNormal(other) {
			t.Errorf("mutation %d should break normal equality", i)
		}
	}
}

func TestStatSignatureOmittedFieldSkipped(t *testing.T) {
	// When ino is omitted on both sides, a differing ino must not break equality.
	a := worktree.StatSignature{Size: 10, MtimeNs: 100, Mode: 0o644, Ino: 2, Omitted: worktree.FieldSet(0).With(worktree.FieldIno).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldNlink)}
	b := a
	b.Ino = 999
	if !a.EqualNormal(b) {
		t.Errorf("omitted ino should be skipped in comparison")
	}
}

func TestStatSignatureEqualFast(t *testing.T) {
	a := worktree.StatSignature{Size: 10, MtimeNs: 100, CtimeNs: 5, Mode: 0o644, Ino: 1}
	b := a
	b.CtimeNs = 999 // ignored by fast
	b.Ino = 999     // ignored by fast
	if !a.EqualFast(b) {
		t.Errorf("fast equality should ignore ctime/ino")
	}
	b.MtimeNs = 101
	if a.EqualFast(b) {
		t.Errorf("fast equality should track mtime")
	}
}

func TestHardlinkIdentityUnknownWhenOmitted(t *testing.T) {
	s := worktree.StatSignature{Dev: 1, Ino: 2, Nlink: 2, Omitted: worktree.FieldSet(0).With(worktree.FieldIno)}
	if s.HardlinkIdentity().Known {
		t.Errorf("hardlink identity should be unknown when ino omitted")
	}
	full := worktree.StatSignature{Dev: 1, Ino: 2, Nlink: 2}
	if !full.HardlinkIdentity().Known {
		t.Errorf("hardlink identity should be known when all fields present")
	}
}

func TestScanIDSortsByTime(t *testing.T) {
	r := bytes.NewReader(make([]byte, 64)) // zero randomness is fine for ordering
	early, err := worktree.NewScanID(1000, r)
	if err != nil {
		t.Fatal(err)
	}
	late, err := worktree.NewScanID(2000, r)
	if err != nil {
		t.Fatal(err)
	}
	if early.String() >= late.String() {
		t.Errorf("scan ids should sort by time: %q !< %q", early, late)
	}
	parsed, err := worktree.ParseScanID(early.String())
	if err != nil {
		t.Fatalf("ParseScanID: %v", err)
	}
	if parsed.String() != early.String() {
		t.Errorf("ParseScanID round trip mismatch")
	}
}

func TestScanIDRejectsBad(t *testing.T) {
	if _, err := worktree.ParseScanID("short"); err == nil {
		t.Errorf("ParseScanID accepted short id")
	}
}

func TestFieldSetValid(t *testing.T) {
	known := worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldNlink)
	if !known.Valid() {
		t.Errorf("a set of known fields should be valid")
	}
	if (worktree.FieldSet(0)).Valid() != true {
		t.Errorf("empty set should be valid")
	}
	// A bit outside the known fields is invalid.
	if worktree.FieldSet(1 << 7).Valid() {
		t.Errorf("a set with an unknown bit should be invalid")
	}
}

func TestTraversalInfoValidate(t *testing.T) {
	src, _ := worktree.ParseRelPath("link")

	good := []worktree.TraversalInfo{
		{}, // directly reached
		{Followed: true, SourcePath: src, Depth: 1}, // followed
	}
	for i, tr := range good {
		if err := tr.Validate(); err != nil {
			t.Errorf("good[%d] rejected: %v", i, err)
		}
	}

	bad := []worktree.TraversalInfo{
		{Followed: true},                  // no source, no depth
		{Followed: true, SourcePath: src}, // depth 0
		{Followed: true, Depth: 1},        // no source
		{SourcePath: src},                 // source but not followed
		{Depth: 2},                        // depth but not followed
	}
	for i, tr := range bad {
		if err := tr.Validate(); err == nil {
			t.Errorf("bad[%d] accepted: %+v", i, tr)
		}
	}
}
