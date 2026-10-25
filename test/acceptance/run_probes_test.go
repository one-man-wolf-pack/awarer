package acceptance

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const probeSentinel = "SENTINEL-identity-report-41d9"

// probeLauncher is a stable launcher outside the project: the bytes awa hashes for
// the project never change, but the toolchain it selects does.
type probeLauncher struct {
	script, selection, calls string
}

func newProbeLauncher(t *testing.T) probeLauncher {
	t.Helper()
	dir := t.TempDir()
	l := probeLauncher{
		script:    filepath.Join(dir, "launcher.sh"),
		selection: filepath.Join(dir, "selected"),
		calls:     filepath.Join(dir, "calls"),
	}
	body := "echo x >> '" + l.calls + "'\nprintf 'tool %s " + probeSentinel + "\\n' \"$(cat '" + l.selection + "')\"\n"
	if err := os.WriteFile(l.script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l.selectVersion(t, "1.0")
	return l
}

func (l probeLauncher) selectVersion(t *testing.T, v string) {
	t.Helper()
	if err := os.WriteFile(l.selection, []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l probeLauncher) callCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(l.calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

func (l probeLauncher) policy() string {
	return validationPolicy("check") + `
[[run.validation.identity_probes]]
name = "tool"
argv = ["sh", "` + l.script + `"]
`
}

func TestIdentityProbesKeyTheRunOnTheSelectedTool(t *testing.T) {
	root := initProject(t)
	u := newUserConfig(t)
	awa := u.awa
	l := newProbeLauncher(t)
	write(t, root, "src.txt", "v1")
	write(t, root, "awa.toml", l.policy())
	runIt := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		return awa(t, root, append(append([]string{"run"}, args...), "--", "sh", "-c", validationBuild)...)
	}

	code, stdout, stderr := runIt(t, "--json")
	if code != 0 || l.callCount(t) != 0 {
		t.Fatalf("ungranted run: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	var denied struct {
		Data struct {
			Cache struct {
				Reason string `json:"reason"`
				Key    string `json:"key"`
			} `json:"cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &denied); err != nil || denied.Data.Cache.Reason != "external-facts-unavailable" {
		t.Fatalf("ungranted run reason = %q (%v)\n%s", denied.Data.Cache.Reason, err, stdout)
	}

	if code, stdout, _ := awa(t, root, "run", "explain", "--", "sh", "-c", validationBuild); code != 0 || !strings.Contains(stdout, "facts:  reuse unavailable: identity probes not approved for this project") || !strings.Contains(stdout, "awa config probes allow") {
		t.Errorf("denied explain does not point to the grant: exit %d\n%s", code, stdout)
	}
	_, stdout, stderr = awa(t, root, "run", "explain", "--json", "--", "sh", "-c", validationBuild)
	var explained struct {
		Data struct {
			Outcome string `json:"outcome"`
			Current struct {
				Key       string `json:"key"`
				Cacheable bool   `json:"cacheable"`
			} `json:"current"`
			ExactMatch struct {
				Assessment string `json:"assessment"`
				Exists     *bool  `json:"exists"`
			} `json:"exact_match"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &explained); err != nil {
		t.Fatalf("explain: %v\n%s\n%s", err, stdout, stderr)
	}
	if e := explained.Data; e.Outcome != "uncached" || e.Current.Cacheable || e.Current.Key != denied.Data.Cache.Key || e.ExactMatch.Assessment != "complete" || e.ExactMatch.Exists == nil || *e.ExactMatch.Exists {
		t.Fatalf("denied explain = %+v, want uncached under the run's key %s", e, denied.Data.Cache.Key)
	}

	u.allow(t, root)
	if l.callCount(t) != 0 {
		t.Fatalf("approving executed %d probes", l.callCount(t))
	}
	if code, _, stderr := runIt(t); code != 0 || isHit(stderr) || l.callCount(t) != 2 || !strings.Contains(stderr, "facts:    1 identity probe reported the same bytes") {
		t.Fatalf("granted miss: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	if code, stdout, stderr := runIt(t); code != 0 || !isHit(stderr) || stdout != "checked\n" || l.callCount(t) != 3 {
		t.Fatalf("hit with a fresh probe: exit %d probes %d stdout %q stderr %q", code, l.callCount(t), stdout, stderr)
	}

	_, stdout, _ = awa(t, root, "run", "show", "--last", "--json")
	for _, want := range []string{`"identity_probes"`, `"before"`, `"after"`, `"status": "acquired"`, `"fingerprint": "blake3:`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run show --json lacks %s:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, probeSentinel) {
		t.Error("run show discloses probe output")
	}

	if code, stdout, stderr := awa(t, root, "status"); code != 0 || strings.Contains(stdout, "none for the current state") || !strings.Contains(stdout, "reusable:    unknown — 1 run(s) need identity facts only awa run acquires") || strings.Contains(stdout, "listing failed") {
		t.Errorf("status without fresh facts claims a verdict: exit %d\n%s\n%s", code, stdout, stderr)
	}
	var st struct {
		Data struct {
			Review struct {
				Runs map[string]json.RawMessage `json:"runs"`
			} `json:"review"`
		} `json:"data"`
	}
	_, stdout, _ = awa(t, root, "status", "--json")
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, stdout)
	}
	for field, want := range map[string]string{"assessment": `"incomplete"`, "reusable": "null", "near": "null", "known_reusable": "0", "unassessed": "1"} {
		if got := string(st.Data.Review.Runs[field]); got != want {
			t.Errorf("status runs.%s = %s, want %s", field, got, want)
		}
	}
	for _, args := range [][]string{
		{"doctor"}, {"config", "effective"}, {"run", "log"}, {"run", "audit"},
		{"run", "show", "--last"},
	} {
		awa(t, root, args...)
	}
	if l.callCount(t) != 3 {
		t.Fatalf("a diagnostic executed %d probes", l.callCount(t)-3)
	}
	for _, args := range [][]string{
		{"run", "explain", "--", "sh", "-c", validationBuild},
		{"run", "explain", "--last"},
	} {
		if code, stdout, stderr := awa(t, root, args...); code != 0 || !strings.Contains(stdout, "cache:  incomplete") || !strings.Contains(stdout, "key:    unavailable") || !strings.Contains(stdout, "exact match: not looked up") {
			t.Errorf("%v without fresh facts: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
	}
	_, stdout, _ = awa(t, root, "run", "explain", "--json", "--", "sh", "-c", validationBuild)
	var incomplete struct {
		Data struct {
			Outcome    string                     `json:"outcome"`
			Reason     string                     `json:"reason"`
			Current    map[string]json.RawMessage `json:"current"`
			ExactMatch map[string]json.RawMessage `json:"exact_match"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &incomplete); err != nil {
		t.Fatalf("incomplete explain: %v\n%s", err, stdout)
	}
	if d := incomplete.Data; d.Outcome != "incomplete" || d.Reason != "external-facts-unacquired" ||
		string(d.Current["key"]) != "null" || string(d.Current["cacheable"]) != "null" || string(d.Current["assessment"]) != `"identity-unavailable"` ||
		string(d.ExactMatch["assessment"]) != `"current-identity-unavailable"` || string(d.ExactMatch["exists"]) != "null" || string(d.ExactMatch["healthy"]) != "null" {
		t.Errorf("incomplete explain JSON = %+v", d)
	}
	for _, args := range [][]string{{"run", "ls", "--json"}, {"run", "ls", "--all", "--json"}, {"run", "ls", "--near", "--json"}} {
		code, stdout, stderr := awa(t, root, args...)
		var ls struct {
			Data struct {
				Assessment string `json:"assessment"`
				Reusable   []struct {
					Reusable bool `json:"reusable"`
				} `json:"reusable"`
				Unassessed []struct {
					Reason string `json:"reason"`
				} `json:"unassessed"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &ls); err != nil || code != 0 {
			t.Fatalf("%v: exit %d %v\n%s", args, code, err, stderr)
		}
		if ls.Data.Assessment != "incomplete" || len(ls.Data.Unassessed) != 1 || ls.Data.Unassessed[0].Reason != "external-facts-unacquired" {
			t.Errorf("%v = %+v, want one unassessed row", args, ls.Data)
		}
		for _, r := range ls.Data.Reusable {
			if r.Reusable {
				t.Errorf("%v lists a reusable row without fresh facts", args)
			}
		}
	}
	if code, stdout, _ := awa(t, root, "run", "ls"); code != 0 || !strings.Contains(stdout, "unassessed (identity facts only awa run acquires):") {
		t.Errorf("human ls: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := awa(t, root, "run", "ls", "--near"); code != 0 || !strings.Contains(stdout, "reusable now:\n  none among the assessed runs\n") || strings.Contains(stdout, "  none\n") {
		t.Errorf("human ls --near claims none across unassessed rows: exit %d\n%s", code, stdout)
	}
	if l.callCount(t) != 3 {
		t.Fatalf("explain, ls or status executed %d probes", l.callCount(t)-3)
	}

	l.selectVersion(t, "2.0")
	if code, _, stderr := runIt(t); code != 0 || isHit(stderr) {
		t.Fatalf("a different selected tool replayed the old result: exit %d stderr %q", code, stderr)
	}

	calls := l.callCount(t)
	if code, _, stderr := awa(t, root, "config", "probes", "revoke"); code != 0 {
		t.Fatalf("revoke: exit %d stderr %q", code, stderr)
	}
	for _, args := range [][]string{
		{"run", "explain", "--json", "--", "sh", "-c", validationBuild},
		{"run", "ls", "--all", "--json"},
		{"run", "ls", "--near", "--json"},
		{"status", "--json"},
	} {
		code, stdout, stderr := awa(t, root, args...)
		if code != 0 || !strings.Contains(stdout, "external-facts-unavailable") || strings.Contains(stdout, "<denied>") {
			t.Errorf("%v under a known denial: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
	}
	_, stdout, _ = awa(t, root, "run", "ls", "--near", "--json")
	if !strings.Contains(stdout, `"matched": []`) || !strings.Contains(stdout, `"differences": []`) {
		t.Errorf("a denied candidate claims a computed comparison:\n%s", stdout)
	}
	code, stdout, stderr = runIt(t, "--json")
	if code != 0 || !strings.Contains(stdout, `"reason": "external-facts-unavailable"`) || strings.Contains(stdout, "<denied>") || !strings.Contains(stdout, `"matched": []`) {
		t.Errorf("denied run inline diagnosis: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if l.callCount(t) != calls {
		t.Fatalf("a diagnostic under a known denial executed %d probes", l.callCount(t)-calls)
	}

	err := filepath.WalkDir(filepath.Join(root, ".awa"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // test fixture path
		if err == nil && strings.Contains(string(b), probeSentinel) {
			t.Errorf("%s stores probe output", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// userConfig is an isolated per-user configuration home, so a test's grants are never
// the operator's real ones. HOME, XDG_CONFIG_HOME and AppData cover where
// os.UserConfigDir looks on each platform.
type userConfig struct{ env []string }

func newUserConfig(t *testing.T) userConfig {
	t.Helper()
	return userConfigAt(t.TempDir())
}

func userConfigAt(home string) userConfig {
	return userConfig{env: []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"AppData=" + filepath.Join(home, "AppData"),
	}}
}

func (u userConfig) awa(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	return awaEnv(t, dir, u.env, args...)
}

func (u userConfig) allow(t *testing.T, root string) string {
	t.Helper()
	code, stdout, stderr := u.awa(t, root, "config", "probes", "allow")
	if code != 0 || !strings.Contains(stdout, "approved identity probes for ") || !strings.Contains(stdout, "unsandboxed") {
		t.Fatalf("allow: exit %d\n%s\n%s", code, stdout, stderr)
	}
	return stdout
}

type probeAuthorizationDoc struct {
	Data struct {
		Origins            map[string]string `json:"origins"`
		ProbeAuthorization *struct {
			Root     string `json:"root"`
			State    string `json:"state"`
			Cause    string `json:"cause"`
			Policies []struct {
				Name     string `json:"name"`
				Approved bool   `json:"approved"`
			} `json:"policies"`
		} `json:"probe_authorization"`
	} `json:"data"`
}

func (u userConfig) authorization(t *testing.T, root string) probeAuthorizationDoc {
	t.Helper()
	code, stdout, stderr := u.awa(t, root, "config", "effective", "--json")
	var doc probeAuthorizationDoc
	if code != 0 || json.Unmarshal([]byte(stdout), &doc) != nil || doc.Data.ProbeAuthorization == nil {
		t.Fatalf("config effective: exit %d\n%s\n%s", code, stdout, stderr)
	}
	for key := range doc.Data.Origins {
		if strings.Contains(key, "probe") && !strings.HasPrefix(key, "run.validation") {
			t.Errorf("authorization leaked into config origins as %q", key)
		}
	}
	return doc
}

func TestNoCheckoutFileCanAuthorizeProbes(t *testing.T) {
	u := newUserConfig(t)
	for _, layer := range []string{".awa/config.toml", "awa.toml"} {
		t.Run(layer, func(t *testing.T) {
			root := initProject(t)
			l := newProbeLauncher(t)
			write(t, root, "awa.toml", l.policy())
			body := "[run]\nallow_identity_probes = true\n"
			if layer == "awa.toml" {
				body = l.policy() + "\n" + body
			}
			write(t, root, layer, body)
			code, _, stderr := u.awa(t, root, "run", "--", "sh", "-c", validationBuild)
			if code != 4 || !strings.Contains(stderr, "allow_identity_probes") || l.callCount(t) != 0 {
				t.Fatalf("a checkout-supplied permission: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
			}
		})
	}
}

func TestProbeGrantIsScopedToTheCanonicalProjectRoot(t *testing.T) {
	u := newUserConfig(t)
	root := initProject(t)
	l := newProbeLauncher(t)
	write(t, root, "src.txt", "v1")
	write(t, root, "awa.toml", l.policy())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink: %v", err)
	}
	runIn := func(dir string) (int, string) {
		t.Helper()
		code, _, stderr := u.awa(t, dir, "run", "--", "sh", "-c", validationBuild)
		return code, stderr
	}

	if doc := u.authorization(t, root); doc.Data.ProbeAuthorization.State != "absent" || len(doc.Data.ProbeAuthorization.Policies) != 1 || doc.Data.ProbeAuthorization.Policies[0].Approved {
		t.Fatalf("before allow = %+v", doc.Data.ProbeAuthorization)
	}
	out := u.allow(t, alias)
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, canon) || !strings.Contains(out, l.script) {
		t.Errorf("allow did not show the canonical root and probe argv:\n%s", out)
	}
	for _, args := range [][]string{{"config", "effective"}, {"config", "validate"}, {"config", "probes", "allow"}} {
		u.awa(t, root, args...)
	}
	if l.callCount(t) != 0 {
		t.Fatalf("grant management or inspection executed %d probes", l.callCount(t))
	}
	doc := u.authorization(t, root)
	if a := doc.Data.ProbeAuthorization; a.State != "recorded" || a.Root != canon || !a.Policies[0].Approved {
		t.Fatalf("after allow via an alias = %+v, want the canonical root approved", a)
	}
	if code, stderr := runIn(root); code != 0 || l.callCount(t) != 2 {
		t.Fatalf("approved run: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	if code, stderr := runIn(alias); code != 0 || !isHit(stderr) || l.callCount(t) != 3 {
		t.Fatalf("run through the alias: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}

	copied := filepath.Join(t.TempDir(), "copy")
	if err := os.CopyFS(copied, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	if code, stderr := runIn(copied); code != 0 || l.callCount(t) != 3 || !strings.Contains(stderr, "not approved for this project") {
		t.Fatalf("a copied project inherited the grant: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}

	write(t, root, "awa.toml", strings.Replace(l.policy(), `name = "tool"`, `name = "renamed"`, 1))
	// Editing awa.toml is an ordinary input change, so this misses and runs both
	// phases; what matters is that the renamed probe is still approved.
	if code, stderr := runIn(root); code != 0 || l.callCount(t) != 5 {
		t.Fatalf("a renamed probe lost approval: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	write(t, root, "awa.toml", strings.Replace(l.policy(), `["sh", "`+l.script, `["sh", "-e", "`+l.script, 1))
	if code, stderr := runIn(root); code != 0 || l.callCount(t) != 5 || !strings.Contains(stderr, "changed since they were approved") {
		t.Fatalf("a changed probe argv kept approval: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	if doc := u.authorization(t, root); doc.Data.ProbeAuthorization.Policies[0].Approved {
		t.Fatal("inspection reports a changed context as approved")
	}

	if code, stdout, _ := u.awa(t, root, "config", "probes", "revoke", "--json"); code != 0 || !strings.Contains(stdout, `"removed": true`) {
		t.Fatalf("revoke: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := u.awa(t, root, "config", "probes", "revoke", "--json"); code != 0 || !strings.Contains(stdout, `"removed": false`) {
		t.Fatalf("second revoke: exit %d\n%s", code, stdout)
	}
	for _, args := range [][]string{{"config", "probes"}, {"config", "probes", "grant"}, {"config", "probes", "allow", "extra"}} {
		if code, _, _ := u.awa(t, root, args...); code != 2 {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
	if l.callCount(t) != 5 {
		t.Fatalf("revoke executed %d probes", l.callCount(t)-5)
	}
}

func TestUnusableProbeGrantStoresNeverAuthorize(t *testing.T) {
	root := initProject(t)
	l := newProbeLauncher(t)
	write(t, root, "src.txt", "v1")
	write(t, root, "awa.toml", l.policy())

	// A home inside the project would let the checkout carry its own grant.
	inside := userConfigAt(filepath.Join(root, "home"))
	if code, stdout, stderr := inside.awa(t, root, "config", "probes", "allow"); code != 1 || strings.Contains(stdout, "approved") || !strings.Contains(stderr, "inside the project") {
		t.Fatalf("allow into the project: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if doc := inside.authorization(t, root); doc.Data.ProbeAuthorization.State != "unavailable" || doc.Data.ProbeAuthorization.Cause != "store-inside-project" {
		t.Fatalf("inspection = %+v", doc.Data.ProbeAuthorization)
	}
	code, stdout, stderr := inside.awa(t, root, "run", "--", "sh", "-c", validationBuild)
	if code != 0 || stdout != "checked\n" || l.callCount(t) != 0 || !strings.Contains(stderr, "grant is unavailable (store-inside-project)") {
		t.Fatalf("run with the store inside the project: exit %d probes %d\n%s\n%s", code, l.callCount(t), stdout, stderr)
	}

	// A home that cannot hold a directory makes allow fail, never report consent.
	file := filepath.Join(t.TempDir(), "home")
	write(t, filepath.Dir(file), "home", "not a directory")
	broken := userConfigAt(file)
	if code, stdout, stderr := broken.awa(t, root, "config", "probes", "allow"); code != 1 || strings.Contains(stdout, "approved") || !strings.Contains(stderr, "the approval is not confirmed") {
		t.Fatalf("allow into an unusable store: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if code, _, _ := broken.awa(t, root, "config", "effective"); code != 0 {
		t.Fatalf("an unusable store failed inspection: exit %d", code)
	}
	if l.callCount(t) != 0 {
		t.Fatalf("an unusable store executed %d probes", l.callCount(t))
	}
}
