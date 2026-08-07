package run_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// childEnvMap parses the environment the fake runner received into name/value pairs.
func childEnvMap(t *testing.T, h *harness) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, kv := range h.runner.gotEnv {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}
	return got
}

// grepDir reports every file under dir whose bytes contain needle, so a test can prove a
// raw environment value never lands anywhere under the project.
func grepDir(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // an unreadable file is not the subject of this test
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return hits
}

// The app-level regression guard: a run service that assembles a child environment with
// no locale at all makes a command decoding UTF-8 under the caller's locale decode
// US-ASCII under awa instead. Removing the locale names from the built-in baseline turns
// this red.
func TestChildInheritsCallerLocale(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["PATH"] = "/usr/bin"
	h.env.vars["LANG"] = "en_US.UTF-8"
	h.env.vars["LC_ALL"] = "en_US.UTF-8"
	h.env.vars["LC_CTYPE"] = "en_US.UTF-8"

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	got := childEnvMap(t, h)
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		if got[name] != "en_US.UTF-8" {
			t.Errorf("child %s = %q, want en_US.UTF-8", name, got[name])
		}
	}
}

// TestChildReceivesTheWrapperMarker proves an executed child always carries exactly the
// fixed advisory marker, including when the caller's own environment tries to state
// something else under that name.
func TestChildReceivesTheWrapperMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ambient map[string]string
	}{
		{"absent", map[string]string{"PATH": "/usr/bin"}},
		{"empty", map[string]string{"PATH": "/usr/bin", "AWA_RUN": ""}},
		{"falsey", map[string]string{"PATH": "/usr/bin", "AWA_RUN": "0"}},
		{"attacker-chosen", map[string]string{"PATH": "/usr/bin", "AWA_RUN": "0; trusted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			defer h.cleanup()
			h.env.vars = tc.ambient

			if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
				t.Fatal(err)
			}

			var markers []string
			for _, kv := range h.runner.gotEnv {
				if name, _, _ := strings.Cut(kv, "="); config.IsReservedEnvName(name) {
					markers = append(markers, kv)
				}
			}
			if want := []string{"AWA_RUN=1"}; !slices.Equal(markers, want) {
				t.Errorf("child marker entries = %v, want %v", markers, want)
			}
		})
	}
}

// TestCacheHitInjectsNothing pins the other half of the marker contract: injection is a
// property of executing a child, and a hit executes none. If a replay ran the command to
// "refresh" anything, the run cache would not be a cache.
func TestCacheHitInjectsNothing(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["PATH"] = "/usr/bin"

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	if h.runner.calls != 1 {
		t.Fatalf("first run executed %d times, want 1", h.runner.calls)
	}

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != runcache.CacheHit {
		t.Fatalf("second identical run status = %s, want hit; the rest of this test proves nothing", res.Status)
	}
	if h.runner.calls != 1 {
		t.Errorf("a cache hit executed the child (%d total calls); a hit must inject nothing because it runs nothing", h.runner.calls)
	}
}

// TestExecutableResolvedUnderTheChildEnvironment pins that the binary awa keys is the
// binary awa runs: resolution must see the same environment — same PATH, same locale,
// same injected facts — that the child will receive. Resolving under awa's own
// environment could key one executable and execute another.
func TestExecutableResolvedUnderTheChildEnvironment(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["PATH"] = "/usr/bin"
	h.env.vars["LANG"] = "en_US.UTF-8"

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(h.resolver.gotEnv, h.runner.gotEnv) {
		t.Errorf("resolution environment differs from the child environment:\n  resolve: %v\n  child:   %v", h.resolver.gotEnv, h.runner.gotEnv)
	}
	if len(h.runner.gotEnv) == 0 {
		t.Error("the child environment is empty; the runner rejects a nil env and the marker is always present")
	}
}

// TestLocaleChangeIsAnHonestMiss proves each locale fact is part of cache identity end to
// end: a run under a different locale must execute rather than replay a result produced
// under the old one, and returning to the original locale must reuse again.
func TestLocaleChangeIsAnHonestMiss(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["PATH"] = "/usr/bin"
	h.env.vars["LC_CTYPE"] = "en_US.UTF-8"

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	h.env.vars["LC_CTYPE"] = "C"
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == runcache.CacheHit {
		t.Error("a run under a different LC_CTYPE reused the previous result; locale changes what a command does, so it must miss")
	}

	h.env.vars["LC_CTYPE"] = "en_US.UTF-8"
	back, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Status != runcache.CacheHit {
		t.Errorf("restoring the original locale gave status %s, want hit; identity must be reproducible, not one-way", back.Status)
	}
}

// TestRawLocaleValueIsNeverPersisted extends the standing privacy proof to the newly
// inherited family: a locale value can be as identifying as any other environment value
// (a path, a custom charset), so it is keyed as a redacted identity and never written.
func TestRawLocaleValueIsNeverPersisted(t *testing.T) {
	const sentinel = "xx_XX.SENTINEL-8c1d0e4a2b"
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["PATH"] = "/usr/bin"
	h.env.vars["LC_CTYPE"] = sentinel

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := childEnvMap(t, h)["LC_CTYPE"]; got != sentinel {
		t.Fatalf("child LC_CTYPE = %q, want the raw value; otherwise this test proves nothing about redaction", got)
	}

	entry, err := h.store.Get(res.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, v := range entry.KeyInput.Env.Vars() {
		if strings.Contains(v.Identity().String(), sentinel) {
			t.Errorf("persisted env var %q carries the raw value", v.Name())
		}
	}
	if hits := grepDir(t, h.rootAbs, sentinel); len(hits) != 0 {
		t.Errorf("raw locale value found on disk in %v", hits)
	}
}
