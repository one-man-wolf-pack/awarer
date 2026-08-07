package config

import (
	"slices"
	"strings"
	"testing"
)

// This file is the standing guard over one decision: which environment variables a
// wrapped command inherits by default. Every name in the built-in baseline is
// inherited by every run in every project without anyone asking for it, so the list
// is a product-wide default with privacy, capability, portability, and cache-identity
// consequences. These tests exist so it can only change deliberately.
//
// The oracle is written out by hand rather than derived from the production lists: a
// test that asks the implementation what the baseline is could only ever agree with it.

// TestBaselineEnvNamesArePinned pins the exact built-in baseline per platform. It fails
// when a name is added, removed, or reordered, which is the point: promoting a variable
// to "inherited by default everywhere" is a reviewed decision, and this test is where the
// review is forced to happen.
func TestBaselineEnvNamesArePinned(t *testing.T) {
	wantCommon := []string{
		// execution: what a command needs in order to run at all
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP",
		// locale: what decides how the command reads, sorts, formats, and reports
		"LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES",
		"LC_MONETARY", "LC_NUMERIC", "LC_TIME",
	}
	wantWindows := append(slices.Clone(wantCommon), "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT")

	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"darwin", wantCommon},
		{"linux", wantCommon},
		{"freebsd", wantCommon},
		{"windows", wantWindows},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			got := BaselineEnvNames(tc.goos)
			if !slices.Equal(got, tc.want) {
				t.Errorf("BaselineEnvNames(%q) =\n  %v\nwant\n  %v\n\nAdding a variable to the built-in baseline makes every run in every project inherit it. Update this pin only together with a recorded review of its execution relevance, secrecy/capability risk, external-state dependency, portability, and false-miss cost.", tc.goos, got, tc.want)
			}
		})
	}
}

// TestBaselineEnvNamesIsIndependentPerCall proves the returned slice is a copy: a caller
// that appends to it must not be able to grow the product default for everyone else.
func TestBaselineEnvNamesIsIndependentPerCall(t *testing.T) {
	// Writing through the returned slice's own backing array is the leak this guards:
	// append into a zero-length reslice reuses that array in place.
	_ = append(BaselineEnvNames("linux")[:0], "MUTATED")
	if got := BaselineEnvNames("linux"); got[0] != "PATH" {
		t.Errorf("BaselineEnvNames leaked its backing array: second call starts with %q", got[0])
	}
}

// envDisposition is the decision recorded for one environment-variable family.
type envDisposition string

const (
	// dispBaseline: inherited by every run without configuration.
	dispBaseline envDisposition = "built-in inherited baseline"
	// dispOptIn: not inherited by default; a project may add it to run.env_allowlist
	// when a command genuinely needs it, accepting the recorded consequence.
	dispOptIn envDisposition = "configured opt-in"
	// dispNeverDefault: must never become a built-in default. Opting in remains the
	// user's decision, but the product never makes it for them.
	dispNeverDefault envDisposition = "never-default"
)

// envFamily is one audited family: what it does, why it was dispositioned that way, and
// the representative names the guard checks.
type envFamily struct {
	name        string
	disposition envDisposition
	// reason is not asserted; it is the recorded evidence for the disposition, kept
	// beside the assertion so the two cannot drift apart.
	reason string
	names  []string
}

// envFamilies is the audit. Every family is present with a disposition, and the
// assertions below hold the implementation to it.
//
// The names here are representatives, not an exhaustive catalogue: the guard proves that
// what is dispositioned as inherited is inherited and that what is not, is not. It does
// not try to enumerate every variable in the world, which is exactly the kind of list
// that rots.
var envFamilies = []envFamily{
	{
		name:        "execution",
		disposition: dispBaseline,
		reason:      "a command cannot run without executable lookup, a home and temp directory, and a shell; these carry no authority beyond what the same-user caller already has, and each is fully keyed",
		names:       []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP"},
	},
	{
		name:        "platform execution (Windows)",
		disposition: dispBaseline,
		reason:      "the Windows equivalents of the same execution needs; absent elsewhere, so they are added only for that platform",
		names:       []string{"SystemRoot", "WINDIR", "COMSPEC", "PATHEXT"},
	},
	{
		name:        "locale",
		disposition: dispBaseline,
		reason:      "dropping them changes program semantics, not preference: encoding, collation, numeric/monetary formatting, and diagnostic language. They carry no authority, are fully keyed as redacted identities, and their false-miss cost is exactly one miss when the caller changes locale — which is a genuinely different run",
		names: []string{
			"LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE",
			"LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
		},
	},
	{
		name:        "message catalogs",
		disposition: dispNeverDefault,
		reason:      "POSIX warns that overriding NLSPATH changes message-catalog lookup and can produce undefined utility behavior; it also names files outside awa's evidence boundary. It is deliberately excluded from the locale promotion",
		names:       []string{"NLSPATH"},
	},
	{
		name:        "time and reproducibility",
		disposition: dispOptIn,
		reason:      "TZ and SOURCE_DATE_EPOCH do change non-interactive bytes, but for most commands the effect is absent, and inheriting a clock-shaping variable by default would add a false-miss cost to every project for a minority benefit. TZDIR additionally names an external directory whose contents awa cannot observe",
		names:       []string{"TZ", "TZDIR", "SOURCE_DATE_EPOCH"},
	},
	{
		name:        "output shaping",
		disposition: dispOptIn,
		reason:      "these describe a terminal, and awa runs the child with a non-terminal stdout it captures; inheriting them would make cache identity depend on the window the caller happened to use. COLUMNS/LINES are the clearest case: a resized terminal must not invalidate a test result",
		names: []string{
			"TERM", "COLORTERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE",
			"FORCE_COLOR", "COLUMNS", "LINES",
		},
	},
	{
		name:        "config and cache roots",
		disposition: dispOptIn,
		reason:      "an XDG root is path-valued: awa can key the path but never the mutable content behind it, so inheriting one by default would silently widen every run's real input surface beyond what the key can prove",
		names:       []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"},
	},
	{
		name:        "toolchains",
		disposition: dispOptIn,
		reason:      "tool-specific and unbounded: awa cannot know each toolchain's contract, and most are either path-valued (same external-state limit as XDG roots) or flag-valued. A project that needs one names it, which is also the moment it accepts the consequence. PYTHONPATH is the worked example: keying the path proves nothing about the importable code behind it, so it is opt-in rather than a shipped default",
		names: []string{
			"GOFLAGS", "GOWORK", "GOMODCACHE", "GOCACHE", "RUSTFLAGS", "CARGO_HOME",
			"RUSTUP_HOME", "VIRTUAL_ENV", "JAVA_HOME", "MAKEFLAGS", "CFLAGS", "LDFLAGS",
			"PYTHONPATH",
		},
	},
	{
		name:        "network and trust",
		disposition: dispNeverDefault,
		reason:      "proxy variables can carry credentials in their URL, certificate variables redirect trust anchors, and registry settings redirect where code is fetched from. None may become a product default",
		names: []string{
			"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY",
			"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NPM_CONFIG_REGISTRY",
		},
	},
	{
		name:        "credentials and capabilities",
		disposition: dispNeverDefault,
		reason:      "these are the secret- and capability-bearing family. An auth socket is a live capability handle, not merely a value, and a cloud profile selector redirects which identity a command acts as",
		names: []string{
			"SSH_AUTH_SOCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE",
			"GOOGLE_APPLICATION_CREDENTIALS", "GITHUB_TOKEN", "DOCKER_HOST", "KUBECONFIG",
		},
	},
	{
		name:        "code and startup injection",
		disposition: dispNeverDefault,
		reason:      "their documented purpose is to make a process load or run code it otherwise would not. Inheriting one by default would let ambient state decide what a supervised command actually executes; `awa doctor` warns when a project opts in anyway",
		names: []string{
			"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "DYLD_INSERT_LIBRARIES",
			"DYLD_LIBRARY_PATH", "BASH_ENV", "ENV", "ZDOTDIR", "NODE_OPTIONS",
			"PYTHONSTARTUP", "RUBYOPT", "PERL5OPT", "JAVA_TOOL_OPTIONS",
		},
	},
	{
		name:        "interactive behavior",
		disposition: dispOptIn,
		reason:      "an editor, pager, or prompt setting shapes an interactive session; awa runs a non-interactive child with stdin on the null device by default, so inheriting them adds key churn without changing what the command does",
		names:       []string{"EDITOR", "VISUAL", "PAGER", "GIT_PAGER", "PS1", "HISTFILE", "SHELLOPTS"},
	},
}

// TestEnvAuditCoversEveryRequiredFamily proves the audit is complete: each family the
// package required has an entry, a disposition, and recorded evidence. A future
// promotion that forgets to classify its family fails here rather than shipping.
func TestEnvAuditCoversEveryRequiredFamily(t *testing.T) {
	required := []string{
		"time and reproducibility",
		"output shaping",
		"platform execution (Windows)",
		"config and cache roots",
		"toolchains",
		"network and trust",
		"credentials and capabilities",
		"code and startup injection",
		"interactive behavior",
	}
	for _, name := range required {
		if !slices.ContainsFunc(envFamilies, func(f envFamily) bool { return f.name == name }) {
			t.Errorf("required environment family %q has no recorded disposition", name)
		}
	}
	for _, f := range envFamilies {
		switch f.disposition {
		case dispBaseline, dispOptIn, dispNeverDefault:
		default:
			t.Errorf("family %q has unknown disposition %q", f.name, f.disposition)
		}
		if len(f.names) == 0 {
			t.Errorf("family %q records no representative names", f.name)
		}
		if len(f.reason) < 40 {
			t.Errorf("family %q records no usable evidence for its disposition", f.name)
		}
	}
}

// TestBaselineMatchesEveryAuditDisposition is the guard with teeth: whatever the audit
// says is inherited must be inherited, and whatever it says is not must be absent from
// the baseline on every platform. Adding an unreviewed variable to the baseline fails
// here even if TestBaselineEnvNamesArePinned were updated carelessly, because the name
// would still have no family claiming it.
func TestBaselineMatchesEveryAuditDisposition(t *testing.T) {
	platforms := []string{"darwin", "linux", "freebsd", "windows"}

	for _, f := range envFamilies {
		for _, name := range f.names {
			for _, goos := range platforms {
				inBaseline := containsFold(BaselineEnvNames(goos), name)
				switch {
				case f.disposition == dispBaseline:
					// The Windows-only family is baseline on Windows and absent elsewhere.
					wantIn := goos == "windows" || f.name != "platform execution (Windows)"
					if inBaseline != wantIn {
						t.Errorf("%s: %q (family %q, %s) in baseline = %v, want %v", goos, name, f.name, f.disposition, inBaseline, wantIn)
					}
				case inBaseline:
					t.Errorf("%s: %q is in the built-in baseline, but family %q is dispositioned %s.\nReason on record: %s", goos, name, f.name, f.disposition, f.reason)
				}
			}
		}
	}
}

// TestEveryBaselineNameIsClaimedByAFamily closes the other direction: a name can only be
// in the baseline if some family owns it. This is what makes an unreviewed addition fail
// even when someone updates the pinned list to match their change.
func TestEveryBaselineNameIsClaimedByAFamily(t *testing.T) {
	claimed := map[string]string{}
	for _, f := range envFamilies {
		if f.disposition != dispBaseline {
			continue
		}
		for _, n := range f.names {
			claimed[strings.ToUpper(n)] = f.name
		}
	}
	for _, name := range BaselineEnvNames("windows") { // the union across platforms
		if _, ok := claimed[strings.ToUpper(name)]; !ok {
			t.Errorf("baseline name %q is claimed by no audited family: classify it (execution relevance, secrecy/capability risk, external-state dependency, portability, false-miss cost) before it ships as a default", name)
		}
	}
}

// TestShippedEnvAllowlistDefaultIsTheReassessedSet pins the configured default, which is
// a weaker but still product-wide decision: these names are not inherited by the baseline,
// yet every project that never edits its config behaves as though they were.
func TestShippedEnvAllowlistDefaultIsTheReassessedSet(t *testing.T) {
	// Re-assessed and kept:
	//   CI        — commonly changes what a non-interactive test/build tool prints and
	//               whether it prompts; no authority; fully keyed.
	//   NODE_ENV  — commonly changes install/build semantics for Node tooling; no
	//               authority; fully keyed.
	//
	// Re-assessed and removed:
	//   PYTHONPATH — path-valued. awa keys the path string but cannot observe the
	//               importable code behind it, so shipping it as a default would let a
	//               project hit on a result whose real inputs had moved. It stays a
	//               valid explicit opt-in; see the "toolchains" family above.
	want := []string{"CI", "NODE_ENV"}
	if got := Defaults().Run.EnvAllowlist; !slices.Equal(got, want) {
		t.Errorf("default run.env_allowlist = %v, want %v — changing a shipped default needs the same review as a baseline change", got, want)
	}
}

// TestReservedMarkerNameIsRejectedByConfig proves the wrapper marker cannot be
// configured. It is awa-owned: a user who could allowlist it could redirect or silence
// what the product states about itself.
func TestReservedMarkerNameIsRejectedByConfig(t *testing.T) {
	for _, spelling := range []string{"AWA_RUN", "awa_run", "Awa_Run"} {
		t.Run(spelling, func(t *testing.T) {
			c := Defaults()
			c.Run.EnvAllowlist = []string{spelling}
			err := c.Validate()
			if err == nil {
				t.Fatalf("config with env_allowlist = [%q] validated; the reserved marker must be rejected on every platform", spelling)
			}
			// The message must name the reservation, not the baseline: the marker is not a
			// redundant inherited name the user should tidy up, it is unconfigurable.
			if !strings.Contains(err.Error(), "reserved by awa") {
				t.Errorf("rejection of %q reads %q, want it to name the reservation", spelling, err)
			}
			if strings.Contains(err.Error(), "already in the built-in baseline") {
				t.Errorf("rejection of %q reused the baseline message, which describes an inherited name rather than an injected one", spelling)
			}
		})
	}
}

// TestReservedMarkerIsNotInherited proves the marker never enters the inherited set. If
// it did, an ambient AWA_RUN would be keyed and passed as though the caller's value were
// awa's own statement.
func TestReservedMarkerIsNotInherited(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "freebsd", "windows"} {
		if containsFold(BaselineEnvNames(goos), WrapperMarkerName) {
			t.Errorf("%s: %q is in the inherited baseline; it must only ever be injected", goos, WrapperMarkerName)
		}
	}
	if !IsReservedEnvName("AWA_RUN") || !IsReservedEnvName("awa_run") {
		t.Error("IsReservedEnvName must recognize the marker regardless of case, so one shared config is valid on every platform")
	}
	if IsReservedEnvName("AWA_RUNNER") || IsReservedEnvName("MY_AWA_RUN") {
		t.Error("IsReservedEnvName must match the exact name, not a prefix or substring")
	}
}

func containsFold(names []string, want string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, want) })
}
