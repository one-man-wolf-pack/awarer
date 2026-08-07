package configfile

import (
	"reflect"
	"strings"
	"testing"

	"awarer/internal/domain/config"
)

// TestEmptyDecodesToDefaults pins an empty config (the on-disk equivalent of an
// absent file once the bytes are read) to config.Defaults(), so any value drift
// fails the build rather than slipping through a partial comparison.
func TestEmptyDecodesToDefaults(t *testing.T) {
	cfg, err := Decode(nil)
	if err != nil {
		t.Fatalf("Decode(nil): %v", err)
	}
	if want := config.Defaults(); !reflect.DeepEqual(cfg, want) {
		t.Errorf("decoded empty config drifted from config.Defaults()\n got: %+v\nwant: %+v", cfg, want)
	}
}

// TestStrictTOMLDecodesToStrict confirms the minimal strict template carries only
// the trust_mode override and otherwise resolves to the product defaults.
func TestStrictTOMLDecodesToStrict(t *testing.T) {
	cfg, err := Decode(StrictTOML())
	if err != nil {
		t.Fatalf("Decode(StrictTOML()): %v", err)
	}
	if want := config.Strict(); !reflect.DeepEqual(cfg, want) {
		t.Errorf("decoded strict config drifted from config.Strict()\n got: %+v\nwant: %+v", cfg, want)
	}
}

func TestDecodeRejectsInvalidEnum(t *testing.T) {
	if _, err := Decode([]byte("[hashing]\ntrust_mode = \"paranoid\"\n")); err == nil {
		t.Error("Decode should reject an unknown trust mode")
	}
}

func TestDecodeRejectsInvalidSize(t *testing.T) {
	if _, err := Decode([]byte("[hashing]\nmax_file_size = \"huge\"\n")); err == nil {
		t.Error("Decode should reject invalid byte size")
	}
}

func TestDecodeRejectsInvalidDuration(t *testing.T) {
	if _, err := Decode([]byte("[run]\nttl = \"soon\"\n")); err == nil {
		t.Error("Decode should reject invalid duration")
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	if _, err := Decode([]byte("[hashing]\nbogus_key = true\n")); err == nil {
		t.Error("Decode should reject unknown field")
	}
}

// TestDecodeNamesEveryUnknownKey pins the one classifier every key outside the
// current schema travels. A spelling the config does not define is simply unknown,
// and the message's job is to point at the line so the fix is mechanical.
//
// The cases are deliberately arbitrary names. Any key this project once accepted
// would take exactly this path — the classifier reads the decoded document shape and
// knows nothing about which unknown names have history — so naming one here would
// only plant the beginnings of a retired-name list in a test whose whole point is
// that no such list exists.
func TestDecodeNamesEveryUnknownKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		toml  string
		wants []string
	}{
		{"top level", "nonsense = 1\n", []string{`unknown key "nonsense"`}},
		{"in a section", "[ui]\ntiem = \"utc\"\n", []string{`unknown key "tiem" in [ui]`}}, //nolint:misspell // deliberate typo: exercises unknown-key rejection
		{"an empty value still counts", "[scope]\nnope = []\n", []string{`unknown key "nope" in [scope]`}},
		{"several at once", "[scope]\nbogus = 1\nnope = [\"x\"]\n",
			[]string{"unknown keys", `"bogus" in [scope]`, `"nope" in [scope]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.toml))
			if err == nil {
				t.Fatalf("Decode accepted %q", tc.toml)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "renamed") || strings.Contains(err.Error(), "instead") {
				t.Errorf("error offers a replacement hint, which requires knowledge of a shape awa no longer honors: %v", err)
			}
		})
	}
}

// TestDecodeMergesExtraExcludes verifies the additive fields compose into the
// effective lists with the documented layering: common reaches both families,
// history-only stays out of run, run-only stays out of history.
func TestDecodeMergesExtraExcludes(t *testing.T) {
	cfg, err := Decode([]byte("[scope]\nextra_excludes = [\"common\"]\n\n[history]\nextra_excludes = [\"histonly\"]\n\n[run]\nextra_excludes = [\"runonly\"]\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	has := func(list []string, v string) bool {
		for _, e := range list {
			if e == v {
				return true
			}
		}
		return false
	}
	histExcludes := cfg.HistoryScanScope().Exclude
	runExcludes := config.EffectiveRunExcludes(cfg.Scope.ExtraExcludes, cfg.Run.ExtraExcludes)
	if !has(histExcludes, "common") || !has(histExcludes, "histonly") {
		t.Errorf("history effective excludes missing extras: %v", histExcludes)
	}
	if has(histExcludes, "runonly") {
		t.Errorf("run-only extra leaked into history excludes: %v", histExcludes)
	}
	if !has(runExcludes, "common") || !has(runExcludes, "runonly") {
		t.Errorf("run effective excludes missing extras: %v", runExcludes)
	}
	if has(runExcludes, "histonly") {
		t.Errorf("history-only extra leaked into run excludes: %v", runExcludes)
	}
}

func TestDecodePartialFillsDefaults(t *testing.T) {
	// A file that sets only one key keeps the defaults for everything else.
	cfg, err := Decode([]byte("[hashing]\ntrust_mode = \"strict\"\n"))
	if err != nil {
		t.Fatalf("Decode partial: %v", err)
	}
	if cfg.Hashing.TrustMode != config.TrustStrict {
		t.Errorf("trust_mode = %v, want strict", cfg.Hashing.TrustMode)
	}
	if cfg.Hashing.MaxFileSize != config.Defaults().Hashing.MaxFileSize {
		t.Errorf("max_file_size = %v, want default", cfg.Hashing.MaxFileSize)
	}
	if cfg.Run.TTL != config.Defaults().Run.TTL {
		t.Errorf("run.ttl = %v, want default", cfg.Run.TTL)
	}
}

func TestDecodeRejectsMalformedTOML(t *testing.T) {
	if _, err := Decode([]byte("this is = = not toml")); err == nil {
		t.Error("Decode should reject malformed TOML")
	}
}
