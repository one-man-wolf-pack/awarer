package acceptance

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// dummyEnvContent is placeholder env content; no fixture here holds a real secret.
const dummyEnvContent = "DUMMY_TOKEN=not-a-secret\n"

// effectiveHashOnly reads the resolved hash_only_patterns list and its origin from
// the built binary's `config effective --json`.
func effectiveHashOnly(t *testing.T, root string) ([]string, string) {
	t.Helper()
	code, out, stderr := awa(t, root, "config", "effective", "--json")
	if code != 0 {
		t.Fatalf("config effective exit = %d, %s", code, stderr)
	}
	var env struct {
		Data struct {
			Checkpoint struct {
				HashOnlyPatterns *[]string `json:"hash_only_patterns"`
			} `json:"checkpoint"`
			Origins map[string]string `json:"origins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode config effective: %v\n%s", err, out)
	}
	if env.Data.Checkpoint.HashOnlyPatterns == nil {
		t.Fatalf("hash_only_patterns missing or null in config effective --json:\n%s", out)
	}
	return *env.Data.Checkpoint.HashOnlyPatterns, env.Data.Origins["checkpoint.hash_only_patterns"]
}

// TestAcceptanceSelectiveRetentionProduction drives the shipped wiring end to end: the
// unspecified default resolves (and is projected) as the env list even when
// store_file_contents = true is explicit, a checkpoint records the selected env file
// hash-only while its template keeps bytes, and an explicit [] is shown and applied as
// a real empty policy.
func TestAcceptanceSelectiveRetentionProduction(t *testing.T) {
	root := initProject(t)
	write(t, root, ".env", dummyEnvContent)
	write(t, root, ".env.example", "DUMMY_TOKEN=\n")
	writeProjectConfig(t, root, "[checkpoint]\nstore_file_contents = true\n")

	want := []string{".env", ".env.*", "*.env", "!.env.example", "!.env.sample", "!.env.template"}
	if got, origin := effectiveHashOnly(t, root); !slices.Equal(got, want) || origin != "default" {
		t.Fatalf("effective hash_only_patterns = %v (origin %q), want the default list from the product default", got, origin)
	}
	if code, out, _ := awa(t, root, "config", "effective"); code != 0 || !strings.Contains(out, "hash_only_patterns = [") {
		t.Errorf("human config effective does not project hash_only_patterns:\n%s", out)
	}

	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %s", code, stderr)
	}
	storage := map[string]any{}
	for _, e := range readOnlyCheckpoint(t, root)["entries"].([]any) {
		m := e.(map[string]any)
		storage[m["path"].(string)] = m["content_storage"]
	}
	if storage[".env"] != "hash-only" || storage[".env.example"] != "blob" {
		t.Errorf("content_storage = %v, want .env hash-only and .env.example blob", storage)
	}
	if bc := blobCount(t, root); bc != 1 {
		t.Errorf("blob count = %d, want 1 (only .env.example)", bc)
	}

	writeProjectConfig(t, root, "[checkpoint]\nhash_only_patterns = []\n")
	if got, origin := effectiveHashOnly(t, root); len(got) != 0 || origin != "local" {
		t.Errorf("explicit [] resolved to %v (origin %q), want [] from local", got, origin)
	}
}

// TestAcceptanceRetentionPolicyLeavesRunIdentityAlone: the private local config is
// outside the scanned tree, so changing only the selection holds every scanned byte
// constant. A run keyed under [] must then be reused under the default list, and
// changing the selected file's bytes must still invalidate it — it remains a run input.
func TestAcceptanceRetentionPolicyLeavesRunIdentityAlone(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, ".env", dummyEnvContent)
	writeProjectConfig(t, root, "[checkpoint]\nhash_only_patterns = []\n")

	status := func() string {
		t.Helper()
		_, out, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
		var env struct {
			Data struct {
				Cache struct {
					Status string `json:"status"`
				} `json:"cache"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("invalid run JSON: %v\n%s", err, out)
		}
		return env.Data.Cache.Status
	}

	if s := status(); s != "miss" {
		t.Fatalf("first run = %q, want miss", s)
	}
	writeProjectConfig(t, root, "")
	if s := status(); s != "hit" {
		t.Errorf("run after a retention-only change = %q, want hit", s)
	}
	write(t, root, ".env", "DUMMY_TOKEN=changed\n")
	if s := status(); s != "miss" {
		t.Errorf("run after changing the selected file = %q, want miss", s)
	}
}

// TestAcceptanceSelectedHashOnlySourceHasNoTextDiff: .env and a.txt hold identical
// bytes, so the store owns that content through a.txt's record. A later diff still
// discloses .env as hash-only — the shared blob is not .env's to read — while a.txt
// gets its ordinary text diff.
func TestAcceptanceSelectedHashOnlySourceHasNoTextDiff(t *testing.T) {
	root := initProject(t)
	write(t, root, ".env", dummyEnvContent)
	write(t, root, "a.txt", dummyEnvContent)
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %s", code, stderr)
	}
	write(t, root, ".env", "DUMMY_TOKEN=changed\n")
	write(t, root, "a.txt", "DUMMY_TOKEN=changed\n")

	code, out, stderr := awa(t, root, "diff", "--json")
	if code != 0 {
		t.Fatalf("diff exit = %d, %s", code, stderr)
	}
	var env struct {
		Data struct {
			Files []struct {
				NewPath      string `json:"new_path"`
				OldPath      string `json:"old_path"`
				Availability string `json:"availability"`
			} `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode diff: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, f := range env.Data.Files {
		p := f.NewPath
		if p == "" {
			p = f.OldPath
		}
		got[p] = f.Availability
	}
	if got[".env"] != "hash-only" || got["a.txt"] != "text" {
		t.Errorf("availability = %v, want .env hash-only and a.txt text", got)
	}
}
