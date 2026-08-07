package lockfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanDirClassifies(t *testing.T) {
	root := t.TempDir()
	locksDir := filepath.Join(root, ".awa", "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// Active: recent record from another host (no owner evidence, within window).
	active, err := Encode(Record{Operation: "checkpoint", PID: 4242, Hostname: "other-host", CreatedAt: now.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "active.lock"), active, 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale: old record from another host, past the stale threshold.
	stale, err := Encode(Record{Operation: "checkpoint", PID: 99, Hostname: "other-host", CreatedAt: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "stale.lock"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	// Unknown: not a lock record.
	if err := os.WriteFile(filepath.Join(locksDir, "garbage.lock"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unknown: a valid lock record with appended trailing bytes. This is the fail-open
	// hole a strict parser closes — it must classify the record unknown so it blocks gc
	// and is never auto-removed, rather than trusting the leading object.
	valid, err := Encode(Record{Operation: "checkpoint", PID: 7, Hostname: "other-host", CreatedAt: now.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	trailing := append(append([]byte{}, valid...), []byte(" trailing junk")...)
	if err := os.WriteFile(filepath.Join(locksDir, "trailing.lock"), trailing, 0o644); err != nil {
		t.Fatal(err)
	}

	never := func(int) bool { return false }
	entries, err := ScanDir(root, locksDir, now, DefaultStaleAfter, "this-host", never)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	got := map[string]EntryState{}
	for _, e := range entries {
		got[e.Name] = e.State
	}
	if got["active.lock"] != EntryActive {
		t.Errorf("active.lock = %v, want active", got["active.lock"])
	}
	if got["stale.lock"] != EntryStale {
		t.Errorf("stale.lock = %v, want stale", got["stale.lock"])
	}
	if got["garbage.lock"] != EntryUnknown {
		t.Errorf("garbage.lock = %v, want unknown", got["garbage.lock"])
	}
	if got["trailing.lock"] != EntryUnknown {
		t.Errorf("trailing.lock = %v, want unknown", got["trailing.lock"])
	}
}

func TestScanDirIsSortedByName(t *testing.T) {
	root := t.TempDir()
	locksDir := filepath.Join(root, ".awa", "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"zebra.lock", "alpha.lock", "mike.lock", "bravo.lock"} {
		if err := os.WriteFile(filepath.Join(locksDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ScanDir(root, locksDir, time.Now(), DefaultStaleAfter, "h", func(int) bool { return true })
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name >= entries[i].Name {
			t.Fatalf("entries not sorted at %d: %q >= %q", i, entries[i-1].Name, entries[i].Name)
		}
	}
}

func TestScanDirAbsentIsClean(t *testing.T) {
	root := t.TempDir()
	locksDir := filepath.Join(root, ".awa", "locks")
	entries, err := ScanDir(root, locksDir, time.Now(), DefaultStaleAfter, "h", func(int) bool { return true })
	if err != nil {
		t.Fatalf("ScanDir absent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
}
