//go:build acceptance_smoke

// The release smoke check: drive the built binary through the handful of
// invocations a release must not be able to break, asserting exit codes and the
// JSON envelope against the real artifact.
//
// It is tagged out of the default suite (run via `just release-gate`, or directly
// with `go test -tags acceptance_smoke -run '^TestSmoke' ./test/acceptance/...`)
// because the rest of this package already covers each behaviour in depth. What
// this adds is breadth over one built binary in one pass: a linker, embed, or
// packaging change that leaves every focused test green while the shipped binary
// cannot print its own version fails here.
//
// It uses the package harness — one build in TestMain, one `awa` helper — and
// decodes the JSON envelope rather than pattern-matching it, which is the whole
// reason this stopped being a shell recipe.
package acceptance

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// envelope is the machine-output contract every --json command shares.
type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
}

// requireEnvelope decodes one JSON response and asserts the three fields that make
// it a valid awa envelope. Decoding is the point: the shell version grepped for
// `"schema_version": 1`, which a reformatted or duplicated key could satisfy
// without the document ever parsing.
func requireEnvelope(t *testing.T, command, stdout string) {
	t.Helper()
	var env envelope
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Errorf("%s --json is not one valid envelope document: %v\n%s", command, err, stdout)
		return
	}
	// Exactly io.EOF, not merely "an error". Anything else means bytes follow the
	// document — and unparseable trailing bytes report a syntax error rather than nil,
	// so accepting any error here would let a valid envelope followed by garbage pass
	// as clean machine output, which is the opposite of what this asserts.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Errorf("%s --json emitted trailing content after the document (%v):\n%s", command, err, stdout)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("%s --json schema_version = %d, want 1", command, env.SchemaVersion)
	}
	if env.Command != command {
		t.Errorf("%s --json command = %q, want %q", command, env.Command, command)
	}
	if len(env.Data) == 0 {
		t.Errorf("%s --json carries no data field:\n%s", command, stdout)
	}
}

// TestSmokeEnvelopeOracleRejectsTrailingContent checks the checker.
//
// requireEnvelope is the only thing standing between "the binary printed something
// JSON-shaped" and "the binary printed valid machine output", so a hole in it makes
// the smoke lane confirm a release it never inspected. The case that hides is a valid
// envelope followed by garbage: the decoder reports a syntax error rather than nil,
// which an "any error means the stream ended" reading accepts as clean.
func TestSmokeEnvelopeOracleRejectsTrailingContent(t *testing.T) {
	const valid = `{"schema_version":1,"command":"version","data":{"v":"1"}}`

	for _, tc := range []struct {
		name, stdout string
		wantFail     bool
	}{
		{"one document", valid, false},
		{"one document with a trailing newline", valid + "\n", false},
		{"followed by unparseable bytes", valid + "\n<<<GARBAGE", true},
		{"followed by a second document", valid + "\n" + valid, true},
		{"followed by a bare token", valid + "\nnull", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &testing.T{}
			requireEnvelope(probe, "version", tc.stdout)
			if probe.Failed() != tc.wantFail {
				t.Errorf("requireEnvelope failed = %v, want %v, for:\n%s", probe.Failed(), tc.wantFail, tc.stdout)
			}
		})
	}
}

// TestSmokeReleaseBinary is the whole smoke lane, in one pass over one binary.
func TestSmokeReleaseBinary(t *testing.T) {
	// Version and help work with no project, no config, and no filesystem state.
	outside := t.TempDir()

	if code, _, stderr := awa(t, outside, "version"); code != 0 {
		t.Fatalf("version exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, _ := awa(t, outside, "version", "--json")
	if code != 0 {
		t.Fatalf("version --json exit = %d", code)
	}
	requireEnvelope(t, "version", stdout)

	if code, _, stderr := awa(t, outside, "help"); code != 0 {
		t.Fatalf("help exit = %d, stderr = %q", code, stderr)
	}
	// Two representative embedded documents: a help topic and a command's flag
	// listing. Both come from the binary's own embedded corpus, so an embed that
	// silently shipped empty is caught here rather than by a reader.
	if code, stdout, _ := awa(t, outside, "help", "agents"); code != 0 || !strings.Contains(stdout, "awa run --record") {
		t.Errorf("help agents exit = %d; it no longer documents `awa run --record`:\n%s", code, stdout)
	}
	if code, stdout, _ := awa(t, outside, "config", "init", "--help"); code != 0 || !strings.Contains(stdout, "--shared") {
		t.Errorf("config init --help exit = %d; it no longer offers --shared:\n%s", code, stdout)
	}

	// A throwaway project, driven through the ordinary first-use path.
	proj := t.TempDir()
	if code, _, stderr := awa(t, proj, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	if code, stdout, _ := awa(t, proj, "status"); code != 0 || !strings.Contains(stdout, "initialized: yes") {
		t.Errorf("status exit = %d, stdout = %q", code, stdout)
	}
	code, stdout, _ = awa(t, proj, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	requireEnvelope(t, "status", stdout)

	if code, _, stderr := awa(t, proj, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	// A usage error stays a usage error in the shipped binary: `run log` takes no
	// --strict, and accepting it silently would be worse than rejecting it loudly.
	if code, _, _ := awa(t, proj, "run", "log", "--strict"); code == 0 {
		t.Error("run log --strict should be a usage error, got exit 0")
	}
	if code, _, stderr := awa(t, proj, "doctor"); code != 0 {
		t.Fatalf("doctor exit = %d, stderr = %q", code, stderr)
	}
}
