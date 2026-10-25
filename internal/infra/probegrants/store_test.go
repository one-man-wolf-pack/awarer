package probegrants

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func testContext(s string) hashing.TreeHash { return blake3hash.New().HashBytes([]byte(s)) }

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := t.TempDir()
	return NewAt(dir, blake3hash.New()), dir, root
}

func recordPath(t *testing.T, s *Store, dir, root string) string {
	t.Helper()
	canon, err := CanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, filepath.FromSlash(grantsDir), s.recordName(canon))
}

func TestAllowCheckRevoke(t *testing.T) {
	s, _, root := newStore(t)
	a, b := testContext("a"), testContext("b")
	if got := s.Check(root, a); got.Granted() || got.Cause != runcache.ProbeCauseNoGrant {
		t.Fatalf("before allow = %+v, want denied no-grant", got)
	}
	canon, err := s.Allow(root, []hashing.TreeHash{b, a, a})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := CanonicalRoot(root); canon != want {
		t.Fatalf("allow root = %q, want %q", canon, want)
	}
	if !s.Check(root, a).Granted() || !s.Check(root, b).Granted() {
		t.Fatal("an approved context was denied")
	}
	if got := s.Check(root, testContext("c")); got.Granted() || got.Cause != runcache.ProbeCauseContextChanged {
		t.Fatalf("unapproved context = %+v", got)
	}
	if _, err := s.Allow(root, []hashing.TreeHash{b}); err != nil {
		t.Fatal(err)
	}
	if s.Check(root, a).Granted() || !s.Check(root, b).Granted() {
		t.Fatal("allow did not replace the record with the current contexts")
	}
	if s.Check(t.TempDir(), b).Granted() {
		t.Fatal("another root inherited the grant")
	}
	if _, removed, err := s.Revoke(root); err != nil || !removed {
		t.Fatalf("revoke = %v %v", removed, err)
	}
	if s.Check(root, b).Granted() {
		t.Fatal("a revoked grant still authorizes")
	}
	if _, removed, err := s.Revoke(root); err != nil || removed {
		t.Fatalf("second revoke = %v %v, want nothing removed", removed, err)
	}
}

func TestRevokeWithoutAStoreRemovesNothing(t *testing.T) {
	s := NewAt(filepath.Join(t.TempDir(), "missing"), blake3hash.New())
	if _, removed, err := s.Revoke(t.TempDir()); err != nil || removed {
		t.Fatalf("revoke = %v %v", removed, err)
	}
}

func TestSymlinkAliasSharesTheGrantAndACopyDoesNot(t *testing.T) {
	s, _, root := newStore(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink: %v", err)
	}
	ctx := testContext("a")
	if _, err := s.Allow(alias, []hashing.TreeHash{ctx}); err != nil {
		t.Fatal(err)
	}
	if !s.Check(root, ctx).Granted() || !s.Check(alias, ctx).Granted() {
		t.Fatal("an alias of the approved root was denied")
	}
	if s.Check(t.TempDir(), ctx).Granted() {
		t.Fatal("a different directory with the same context was granted")
	}
}

func TestDamagedRecordsNeverAuthorize(t *testing.T) {
	ctx := testContext("a")
	cases := map[string]func(rootCanon string) string{
		"not json": func(string) string { return "{" },
		"unknown field": func(r string) string {
			return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `"],"allow":true}`
		},
		"other root": func(string) string { return `{"version":1,"root":"/elsewhere","contexts":["` + ctx.String() + `"]}` },
		"newer version": func(r string) string {
			return `{"version":2,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `"]}`
		},
		"no contexts": func(r string) string { return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":[]}` },
		"bad context": func(r string) string { return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["true"]}` },
		"unsorted": func(r string) string {
			return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `","` + ctx.String() + `"]}`
		},
		"trailing value": func(r string) string {
			return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `"]} {}`
		},
		"trailing bracket": func(r string) string {
			return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `"]}]`
		},
		"trailing brace": func(r string) string {
			return `{"version":1,"root":"` + jsonEscape(r) + `","contexts":["` + ctx.String() + `"]}}`
		},
		"oversize": func(string) string { return strings.Repeat(" ", maxRecordBytes+1) },
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, dir, root := newStore(t)
			canon, _ := CanonicalRoot(root)
			p := recordPath(t, s, dir, root)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body(canon)), 0o600); err != nil {
				t.Fatal(err)
			}
			got := s.Check(root, ctx)
			if got.Granted() || got.State != runcache.ProbeUnavailable || got.Cause != runcache.ProbeCauseStoreInvalid {
				t.Fatalf("check = %+v, want unavailable store-invalid", got)
			}
		})
	}
}

func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

func TestUnreadableRecordNeverAuthorizes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not deny this reader")
	}
	s, dir, root := newStore(t)
	ctx := testContext("a")
	if _, err := s.Allow(root, []hashing.TreeHash{ctx}); err != nil {
		t.Fatal(err)
	}
	p := recordPath(t, s, dir, root)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if got := s.Check(root, ctx); got.Granted() || got.Cause != runcache.ProbeCauseStoreUnreadable {
		t.Fatalf("check = %+v, want unavailable store-unreadable", got)
	}
}

func TestSymlinkedRecordNeverAuthorizes(t *testing.T) {
	s, dir, root := newStore(t)
	ctx := testContext("a")
	other := t.TempDir()
	if _, err := s.Allow(other, []hashing.TreeHash{ctx}); err != nil {
		t.Fatal(err)
	}
	p := recordPath(t, s, dir, root)
	if err := os.Symlink(recordPath(t, s, dir, other), p); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got := s.Check(root, ctx); got.Granted() {
		t.Fatalf("a symlinked record authorized: %+v", got)
	}
}

func TestAStoreInsideTheProjectNeverAuthorizes(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "home", ".config")
	s := NewAt(inside, blake3hash.New())
	ctx := testContext("a")
	if _, err := s.Allow(root, []hashing.TreeHash{ctx}); err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Fatalf("allow inside the project = %v, want a refusal", err)
	}
	// A record the checkout supplied at that location is not consent either.
	outside := NewAt(t.TempDir(), blake3hash.New())
	if _, err := outside.Allow(root, []hashing.TreeHash{ctx}); err != nil {
		t.Fatal(err)
	}
	canon, _ := CanonicalRoot(root)
	data, err := os.ReadFile(filepath.Join(outside.userDirMust(t), filepath.FromSlash(grantsDir), s.recordName(canon)))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(inside, filepath.FromSlash(grantsDir), s.recordName(canon))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Check(root, ctx); got.Granted() || got.Cause != runcache.ProbeCauseStoreInsideProject {
		t.Fatalf("check = %+v, want unavailable store-inside-project", got)
	}
	if _, _, err := s.Revoke(root); err == nil {
		t.Fatal("revoke inside the project reported success")
	}
}

func (s *Store) userDirMust(t *testing.T) string {
	t.Helper()
	d, err := s.userDir()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWriteFailureReportsNoGrant(t *testing.T) {
	base := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAt(base, blake3hash.New())
	root := t.TempDir()
	ctx := testContext("a")
	if _, err := s.Allow(root, []hashing.TreeHash{ctx}); err == nil {
		t.Fatal("allow into an unusable store reported success")
	}
	if s.Check(root, ctx).Granted() {
		t.Fatal("a failed write authorized")
	}
}

func TestUnresolvedRootNeverAuthorizes(t *testing.T) {
	s, _, _ := newStore(t)
	missing := filepath.Join(t.TempDir(), "gone")
	if got := s.Check(missing, testContext("a")); got.Granted() || got.Cause != runcache.ProbeCauseUnresolvedRoot {
		t.Fatalf("check = %+v, want unavailable unresolved-root", got)
	}
	if _, err := s.Allow(missing, []hashing.TreeHash{testContext("a")}); err == nil {
		t.Fatal("allow for an unresolved root reported success")
	}
}

func TestTrailingWhitespaceAfterTheRecordIsAccepted(t *testing.T) {
	s, dir, root := newStore(t)
	ctx := testContext("a")
	canon, _ := CanonicalRoot(root)
	p := recordPath(t, s, dir, root)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"root":"` + jsonEscape(canon) + `","contexts":["` + ctx.String() + `"]}` + " \n\t\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Check(root, ctx); !got.Granted() {
		t.Fatalf("check = %+v, want granted", got)
	}
}

// A project whose tree holds the grant directory itself (its root is <user-config>/awa
// or the grant directory) could ship the record, even though the user-config base is
// outside it.
func TestAProjectContainingTheGrantDirectoryCannotSupplyItsGrant(t *testing.T) {
	for _, rel := range []string{"awa", "awa/probe-grants"} {
		t.Run(rel, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, filepath.FromSlash(rel))
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			s := NewAt(base, blake3hash.New())
			ctx := testContext("a")
			canon, err := CanonicalRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			p := recordPath(t, s, base, root)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"version":1,"root":"` + jsonEscape(canon) + `","contexts":["` + ctx.String() + `"]}`
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.Check(root, ctx); got.Granted() || got.Cause != runcache.ProbeCauseStoreInsideProject {
				t.Fatalf("checkout-supplied record = %+v, want unavailable store-inside-project", got)
			}
			if _, err := s.Allow(root, []hashing.TreeHash{ctx}); err == nil || !strings.Contains(err.Error(), "inside the project") {
				t.Fatalf("allow = %v, want a refusal", err)
			}
			if _, _, err := s.Revoke(root); err == nil || !strings.Contains(err.Error(), "inside the project") {
				t.Fatalf("revoke = %v, want a refusal", err)
			}
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("a refused revoke touched the checkout's file: %v", err)
			}
		})
	}
}
