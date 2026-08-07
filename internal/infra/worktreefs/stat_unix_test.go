//go:build darwin || linux || freebsd

package worktreefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"awarer/internal/domain/worktree"
)

// TestStatSignatureCarriesEveryUnixField proves the signature this platform produces is
// complete: ctime, dev, ino, and nlink are read from the real syscall.Stat_t and nothing
// is recorded as omitted.
//
// The reuse decision is what rests on it. An omitted field is skipped by comparison
// rather than treated as a zero, so a platform that quietly stopped populating one would
// not fail anything — it would silently compare on less evidence, and the coarser
// signature is invisible until a changed file is reused as unchanged. The hardlink and
// second-file cases are here for the same reason: a field left at its zero value still
// satisfies a mapping check against a fixture whose own value happens to be zero, so the
// fixture's raw values are asserted distinguishable first.
func TestStatSignatureCarriesEveryUnixField(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	link := filepath.Join(dir, "link")

	before := time.Now()
	if err := os.WriteFile(first, []byte("first payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	after := time.Now()

	sigFirst, stFirst := signatureAndStat(t, first)
	sigSecond, stSecond := signatureAndStat(t, second)

	if sigFirst.Omitted != worktree.FieldSet(0) {
		t.Errorf("Omitted = %v, want an empty set: this platform supplies every field, and a "+
			"recorded omission makes comparison skip it", sigFirst.Omitted.Tokens())
	}

	// The fixture must be able to tell a populated field from a zeroed one.
	if stFirst.Dev == 0 || stFirst.Ino == 0 {
		t.Fatalf("the fixture cannot discriminate: raw dev = %d, ino = %d, both must be non-zero",
			stFirst.Dev, stFirst.Ino)
	}

	if sigFirst.Dev != uint64(stFirst.Dev) { //nolint:unconvert // required on darwin (int32)
		t.Errorf("Dev = %d, want the raw st_dev %d", sigFirst.Dev, stFirst.Dev)
	}
	if sigFirst.Dev != sigSecond.Dev {
		t.Errorf("two files in one directory report different devices (%d, %d)", sigFirst.Dev, sigSecond.Dev)
	}
	if sigFirst.Ino != stFirst.Ino {
		t.Errorf("Ino = %d, want the raw st_ino %d", sigFirst.Ino, stFirst.Ino)
	}
	if sigFirst.Ino == sigSecond.Ino {
		t.Errorf("two distinct files share inode %d; the field is not being read", sigFirst.Ino)
	}
	if sigSecond.Ino != stSecond.Ino {
		t.Errorf("Ino = %d for the second file, want the raw st_ino %d", sigSecond.Ino, stSecond.Ino)
	}

	// The hard link is the behavioural half of nlink: a constant would pass the mapping
	// check but not this one.
	if sigFirst.Nlink != 2 {
		t.Errorf("Nlink = %d for a file with one hard link, want 2", sigFirst.Nlink)
	}
	if sigSecond.Nlink != 1 {
		t.Errorf("Nlink = %d for an unlinked file, want 1", sigSecond.Nlink)
	}

	if sigFirst.CtimeNs != statCtimeNs(stFirst) {
		t.Errorf("CtimeNs = %d, want the platform's change time %d", sigFirst.CtimeNs, statCtimeNs(stFirst))
	}
	// Filesystem timestamp granularity can be as coarse as a second, so the window is
	// widened rather than the assertion dropped: what it must exclude is a zero or a
	// value from some other clock.
	lo, hi := before.Add(-time.Second).UnixNano(), after.Add(time.Second).UnixNano()
	if sigFirst.CtimeNs < lo || sigFirst.CtimeNs > hi {
		t.Errorf("CtimeNs = %d is outside the window this file was created in [%d, %d]",
			sigFirst.CtimeNs, lo, hi)
	}

	// Mode is the raw st_mode the manifest persists, not Go's abstract FileMode bits.
	if sigFirst.Mode != uint32(stFirst.Mode) { //nolint:unconvert // required on darwin and freebsd (uint16)
		t.Errorf("Mode = %#o, want the raw st_mode %#o", sigFirst.Mode, stFirst.Mode)
	}
	if sigFirst.Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Errorf("Mode = %#o does not carry the regular-file type bits; the abstract FileMode "+
			"bits would not either, which is how this catches the wrong source", sigFirst.Mode)
	}

	if want := int64(len("first payload")); sigFirst.Size != want {
		t.Errorf("Size = %d, want %d", sigFirst.Size, want)
	}
	if sigFirst.MtimeNs == 0 {
		t.Error("MtimeNs is zero")
	}
}

// signatureAndStat returns the signature under test beside the raw stat it must reflect.
func signatureAndStat(t *testing.T, path string) (worktree.StatSignature, *syscall.Stat_t) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: FileInfo.Sys() is %T, not *syscall.Stat_t; this platform compiles the "+
			"native signature but cannot supply it", path, info.Sys())
	}
	return StatSignatureOf(info), st
}

// TestStatSignatureRecordsOmissionRatherThanGuessing covers the other branch: a FileInfo
// carrying no platform stat must mark all four fields omitted rather than report them as
// zero, because comparison skips an omitted field and trusts a present one.
func TestStatSignatureRecordsOmissionRatherThanGuessing(t *testing.T) {
	sig := StatSignatureOf(statlessInfo{})

	for _, f := range []worktree.StatField{
		worktree.FieldCtime, worktree.FieldDev, worktree.FieldIno, worktree.FieldNlink,
	} {
		if !sig.Omitted.Has(f) {
			t.Errorf("%s is not marked omitted, so comparison would trust a zero", f)
		}
	}
	if sig.CtimeNs != 0 || sig.Dev != 0 || sig.Ino != 0 || sig.Nlink != 0 {
		t.Errorf("an unavailable field was given a value: ctime=%d dev=%d ino=%d nlink=%d",
			sig.CtimeNs, sig.Dev, sig.Ino, sig.Nlink)
	}
}

// statlessInfo is a FileInfo whose Sys() carries no platform stat.
type statlessInfo struct{}

func (statlessInfo) Name() string       { return "stateless" }
func (statlessInfo) Size() int64        { return 3 }
func (statlessInfo) Mode() fs.FileMode  { return 0o644 }
func (statlessInfo) ModTime() time.Time { return time.Unix(1, 0) }
func (statlessInfo) IsDir() bool        { return false }
func (statlessInfo) Sys() any           { return nil }
