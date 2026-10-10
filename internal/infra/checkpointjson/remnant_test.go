package checkpointjson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/scantest"
)

func stoppedPublication(t *testing.T, layout paths.Layout, b byte) checkpoint.CheckpointID {
	t.Helper()
	repo := NewRepo(layout)
	repo.fail = func(stage failStage) error {
		if stage == failAfterManifestBeforeHeader {
			return errors.New("stopped before the header")
		}
		return nil
	}
	s := buildCheckpoint(t, idFrom(t, b), time.Unix(int64(b), 0).UTC(), false)
	if _, err := repo.PutManifest(context.Background(), s.build, s.stream(), scantest.FrontierStream()); err == nil {
		t.Fatal("PutManifest reached the header; the fixture is not a stopped publication")
	}
	return s.id()
}

func committedCheckpoint(t *testing.T, layout paths.Layout, b byte) checkpoint.CheckpointID {
	t.Helper()
	s := buildCheckpoint(t, idFrom(t, b), time.Unix(int64(b), 0).UTC(), false)
	if err := s.putErr(NewRepo(layout)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	return s.id()
}

func emptyIDDir(t *testing.T, layout paths.Layout, b byte) checkpoint.CheckpointID {
	t.Helper()
	id := idFrom(t, b)
	if err := os.MkdirAll(filepath.Join(layout.CheckpointsDir(), id.String()), paths.DirPerm); err != nil {
		t.Fatal(err)
	}
	return id
}

func plant(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		desc := info.Mode().Type().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			desc += ":" + string(data)
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func assertSameTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := treeOf(t, dir)
	if len(got) != len(want) {
		t.Fatalf("%s changed: has %d nodes, had %d", dir, len(got), len(want))
	}
	for name, desc := range want {
		if got[name] != desc {
			t.Fatalf("%s changed at %q", dir, name)
		}
	}
}

func headerless(t *testing.T, repo *Repo) map[checkpoint.CheckpointID]Remnant {
	t.Helper()
	out := map[checkpoint.CheckpointID]Remnant{}
	err := repo.EachHeaderless(context.Background(), func(r Remnant) error {
		out[r.ID] = r
		return nil
	})
	if err != nil {
		t.Fatalf("EachHeaderless: %v", err)
	}
	return out
}

func TestHeaderlessRemnantEligibility(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, layout paths.Layout) checkpoint.CheckpointID
		want  RemnantState
	}{
		{"empty directory", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			return emptyIDDir(t, l, 0x11)
		}, RemnantEligible},
		{"stopped publication payloads", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			return stoppedPublication(t, l, 0x12)
		}, RemnantEligible},
		{"publisher temporary files", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := stoppedPublication(t, l, 0x13)
			dir := filepath.Join(l.CheckpointsDir(), id.String())
			plant(t, dir, ".tmp-4242-7")
			plant(t, dir, ".tmp-1234567890")
			if err := os.Link(filepath.Join(dir, manifestName), filepath.Join(dir, ".tmp-4242-8")); err != nil {
				t.Fatal(err)
			}
			return id
		}, RemnantEligible},
		{"temporary file only", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := emptyIDDir(t, l, 0x14)
			plant(t, filepath.Join(l.CheckpointsDir(), id.String()), ".tmp-1-1")
			return id
		}, RemnantEligible},
		{"unknown extra file", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := stoppedPublication(t, l, 0x15)
			dir := filepath.Join(l.CheckpointsDir(), id.String())
			plant(t, dir, ".tmp-4242-7")
			plant(t, dir, "notes.txt")
			return id
		}, RemnantUnsupported},
		{"temporary-looking foreign name", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := stoppedPublication(t, l, 0x16)
			plant(t, filepath.Join(l.CheckpointsDir(), id.String()), ".tmp-manifest")
			return id
		}, RemnantUnsupported},
		{"nested directory", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := stoppedPublication(t, l, 0x17)
			if err := os.Mkdir(filepath.Join(l.CheckpointsDir(), id.String(), "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			return id
		}, RemnantUnsupported},
		{"directory at a publication name", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := emptyIDDir(t, l, 0x18)
			nested := filepath.Join(l.CheckpointsDir(), id.String(), manifestName)
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			plant(t, nested, "kept")
			return id
		}, RemnantUnsupported},
		{"symlink at a publication name", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := emptyIDDir(t, l, 0x19)
			dir := filepath.Join(l.CheckpointsDir(), id.String())
			plant(t, dir, manifestName)
			mustSymlink(t, manifestName, filepath.Join(dir, frontierName))
			return id
		}, RemnantUnsupported},
		{"more files than a publication leaves", func(t *testing.T, l paths.Layout) checkpoint.CheckpointID {
			id := emptyIDDir(t, l, 0x1a)
			for i := 0; i <= maxRemnantFiles; i++ {
				plant(t, filepath.Join(l.CheckpointsDir(), id.String()), ".tmp-1-"+strconv.Itoa(i))
			}
			return id
		}, RemnantUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout := paths.New(t.TempDir())
			repo := NewRepo(layout)
			neighbour := committedCheckpoint(t, layout, 0x70)
			neighbourDir := filepath.Join(layout.CheckpointsDir(), neighbour.String())
			neighbourBefore := treeOf(t, neighbourDir)
			id := c.build(t, layout)
			dir := filepath.Join(layout.CheckpointsDir(), id.String())
			before := treeOf(t, dir)

			seen := headerless(t, repo)
			if len(seen) != 1 || seen[id].State != c.want {
				t.Fatalf("EachHeaderless = %+v, want only %s in state %d", seen, id, c.want)
			}
			if seen[id].Path != dir {
				t.Errorf("Path = %q, want %q", seen[id].Path, dir)
			}
			assertSameTree(t, dir, before)

			rem, err := repo.RemoveHeaderless(context.Background(), id)
			if err != nil {
				t.Fatalf("RemoveHeaderless: %v", err)
			}
			if rem.State != c.want {
				t.Fatalf("RemoveHeaderless state = %d (%s), want %d", rem.State, rem.Detail, c.want)
			}
			if c.want == RemnantEligible {
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("eligible remnant still present: %v", err)
				}
				if again, err := repo.RemoveHeaderless(context.Background(), id); err != nil || again.State != RemnantAbsent {
					t.Fatalf("second removal = state %d, err %v; want absent", again.State, err)
				}
			} else {
				if rem.Detail == "" {
					t.Error("an unsupported remnant names no obstruction")
				}
				assertSameTree(t, dir, before)
			}

			assertSameTree(t, neighbourDir, neighbourBefore)
			if err := readCheckpoint(repo, neighbour); err != nil {
				t.Fatalf("adjacent committed checkpoint unreadable: %v", err)
			}
		})
	}
}

func TestPresentHeaderIsNeverARemnant(t *testing.T) {
	cases := []struct {
		name    string
		damage  func(t *testing.T, headerPath string)
		listErr bool
	}{
		{"committed", func(*testing.T, string) {}, false},
		{"corrupt", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"empty", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(p, 0); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"incompatible", func(t *testing.T, p string) { bumpHeaderSchema(t, p, 99) }, false},
		{"symlinked", func(t *testing.T, p string) {
			moved := filepath.Join(filepath.Dir(filepath.Dir(p)), "moved-header")
			if err := os.Rename(p, moved); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, moved, p)
		}, true},
		{"directory", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout := paths.New(t.TempDir())
			repo := NewRepo(layout)
			id := committedCheckpoint(t, layout, 0x21)
			dir := filepath.Join(layout.CheckpointsDir(), id.String())
			c.damage(t, filepath.Join(dir, headerName))
			before := treeOf(t, dir)

			var seen []Remnant
			err := repo.EachHeaderless(context.Background(), func(r Remnant) error {
				seen = append(seen, r)
				return nil
			})
			if (err != nil) != c.listErr {
				t.Fatalf("EachHeaderless error = %v, want error %v", err, c.listErr)
			}
			if len(seen) != 0 {
				t.Fatalf("a record with a present header was reported headerless: %+v", seen)
			}

			rem, err := repo.RemoveHeaderless(context.Background(), id)
			if err != nil {
				t.Fatalf("RemoveHeaderless: %v", err)
			}
			if rem.State != RemnantHeaderPresent {
				t.Fatalf("state = %d, want header present", rem.State)
			}
			assertSameTree(t, dir, before)
		})
	}
}

func TestRemoveHeaderlessDecidesFromCurrentState(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := committedCheckpoint(t, layout, 0x31)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	aside := filepath.Join(layout.Root(), "header.aside")
	if err := os.Rename(filepath.Join(dir, headerName), aside); err != nil {
		t.Fatal(err)
	}
	if got := headerless(t, repo)[id].State; got != RemnantEligible {
		t.Fatalf("pre-commit state = %d, want eligible", got)
	}

	if err := os.Rename(aside, filepath.Join(dir, headerName)); err != nil {
		t.Fatal(err)
	}
	rem, err := repo.RemoveHeaderless(context.Background(), id)
	if err != nil || rem.State != RemnantHeaderPresent {
		t.Fatalf("RemoveHeaderless after commit = state %d, err %v; want header present", rem.State, err)
	}
	if err := readCheckpoint(repo, id); err != nil {
		t.Fatalf("checkpoint committed before removal is unreadable: %v", err)
	}
}

func TestRemoveHeaderlessPartialFailureIsNotRemovalAndRetries(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := stoppedPublication(t, layout, 0x41)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())

	boom := errors.New("removal stopped")
	repo.fail = func(stage failStage) error {
		if stage == failAfterRemnantFileRemoved {
			return boom
		}
		return nil
	}
	if _, err := repo.RemoveHeaderless(context.Background(), id); !errors.Is(err, boom) {
		t.Fatalf("RemoveHeaderless error = %v, want the injected failure", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("a failed removal must leave the directory: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("after one removal the directory holds %d entries, want 1", len(left))
	}

	repo.fail = nil
	if got := headerless(t, repo)[id].State; got != RemnantEligible {
		t.Fatalf("remaining state = %d, want eligible", got)
	}
	rem, err := repo.RemoveHeaderless(context.Background(), id)
	if err != nil || rem.State != RemnantEligible {
		t.Fatalf("retry = state %d, err %v", rem.State, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left the directory: %v", err)
	}
}

func TestRemoveHeaderlessHonorsCancellation(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := stoppedPublication(t, layout, 0x51)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	before := treeOf(t, dir)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.RemoveHeaderless(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled removal error = %v", err)
	}
	assertSameTree(t, dir, before)
	if err := repo.EachHeaderless(cancelled, func(Remnant) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled walk error = %v", err)
	}

	ctx, cancelMid := context.WithCancel(context.Background())
	repo.fail = func(stage failStage) error {
		if stage == failAfterRemnantFileRemoved {
			cancelMid()
		}
		return nil
	}
	if _, err := repo.RemoveHeaderless(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-removal cancellation error = %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("a cancelled removal must leave the directory: %v", err)
	}
}

func TestEachHeaderlessReportsEveryRemnantOnce(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	committedCheckpoint(t, layout, 0x61)
	want := []string{
		emptyIDDir(t, layout, 0x62).String(),
		stoppedPublication(t, layout, 0x63).String(),
	}
	if err := os.Mkdir(filepath.Join(layout.CheckpointsDir(), "not-an-id"), 0o700); err != nil {
		t.Fatal(err)
	}
	var got []string
	for id := range headerless(t, repo) {
		got = append(got, id.String())
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("EachHeaderless ids = %v, want %v", got, want)
	}
}
