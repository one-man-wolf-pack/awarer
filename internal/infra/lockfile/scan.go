package lockfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"awarer/internal/infra/fsx"
)

// maxLockFileSize bounds how much of a lock file the scanner reads. A recognized
// lock record is tiny; anything larger is not one of ours, so an oversized file is
// rejected (and classified unknown) rather than parsed from a truncated prefix —
// which could otherwise accept valid JSON in the first bytes and silently drop
// trailing garbage.
const maxLockFileSize = 64 * 1024

// EntryState is how the scanner classified one file in the locks directory.
type EntryState int

const (
	// EntryActive: a recognized record whose owner appears live.
	EntryActive EntryState = iota + 1
	// EntryStale: a recognized record provably safe to remove.
	EntryStale
	// EntryUnknown: a file the scanner could not recognize as a lock record (a
	// directory, an unreadable file, an unsupported schema, or malformed JSON). It
	// is never a removal target — discarding it could drop a live writer's
	// protection.
	EntryUnknown
)

func (s EntryState) String() string {
	switch s {
	case EntryActive:
		return "active"
	case EntryStale:
		return "stale"
	case EntryUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// ScanEntry is one classified file under .awa/locks. Record is meaningful only when
// State is EntryActive or EntryStale; Detail explains an EntryUnknown.
type ScanEntry struct {
	Name   string
	Path   string
	State  EntryState
	Record Record
	Detail string
}

// ScanDir lists locksDir under the project root no-follow and classifies every
// entry as active, stale, or unknown, using the same parse-and-classify path doctor
// and GC rely on. An absent locks directory is a clean (nil, nil) result. A
// symlinked or unreadable locks directory is returned as an error for the caller to
// interpret (doctor reports that through its layout check). now/staleAfter/hostname/
// alive are injected so classification is testable without real time or processes.
func ScanDir(root, locksDir string, now time.Time, staleAfter time.Duration, hostname string, alive func(pid int) bool) ([]ScanEntry, error) {
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return nil, err
	}
	entries, err := fsx.ReadDirNoFollow(root, relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// Directory reads are not ordered; sort by name so callers (doctor findings, gc
	// candidates) see a deterministic, stable listing.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]ScanEntry, 0, len(entries))
	for _, e := range entries {
		p := filepath.Join(locksDir, e.Name())
		se := ScanEntry{Name: e.Name(), Path: p, State: EntryUnknown}
		if e.IsDir() {
			se.Detail = "unrecognized lock entry (a directory)"
			out = append(out, se)
			continue
		}
		data, err := readLockFile(root, locksDir, e.Name())
		if err != nil {
			se.Detail = "cannot read lock file: " + err.Error()
			out = append(out, se)
			continue
		}
		rec, err := Parse(data)
		if err != nil {
			se.Detail = "unrecognized lock record: " + err.Error()
			out = append(out, se)
			continue
		}
		se.Record = rec
		switch rec.Classify(now, staleAfter, hostname, alive) {
		case StatusStale:
			se.State = EntryStale
		default:
			se.State = EntryActive
		}
		out = append(out, se)
	}
	return out, nil
}

// readLockFile reads one lock file no-follow, capped at maxLockFileSize so an
// oversized file is rejected rather than parsed from a truncated prefix.
func readLockFile(root, locksDir, name string) ([]byte, error) {
	rel, err := fsx.RelUnder(root, filepath.Join(locksDir, name))
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(root, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Read one byte past the cap to detect an oversized file without loading it whole.
	data, err := io.ReadAll(io.LimitReader(f, maxLockFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxLockFileSize {
		return nil, errors.New("lock file is too large to be a lock record")
	}
	return data, nil
}
