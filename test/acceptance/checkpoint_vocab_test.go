package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

// The product language: the canonical command is "awa checkpoint", its human output
// names the object a "checkpoint", and it still prints the full copyable id plus the
// follow-up ranges a reviewer needs for a long fix loop.
func TestCheckpointCanonicalCommand(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc\n")

	code, stdout, stderr := awa(t, root, "checkpoint", "-m", "canonical checkpoint")
	if code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "created checkpoint ") {
		t.Errorf("checkpoint headline must name a checkpoint:\n%s", stdout)
	}
	for _, want := range []string{"id:", "compare:", "diff:", "..now"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("checkpoint output missing %q:\n%s", want, stdout)
		}
	}
}

// TestCheckpointJSONEnvelopeCommand pins the machine contract: the --json envelope
// command token matches the canonical command name "checkpoint", and the document
// carries the full id and no human relative-time prose.
func TestCheckpointJSONEnvelopeCommand(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc\n")

	code, stdout, stderr := awa(t, root, "checkpoint", "--json", "-m", "json checkpoint")
	if code != 0 {
		t.Fatalf("checkpoint --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Command       string          `json:"command"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("checkpoint --json is not one JSON document: %v\n%s", err, stdout)
	}
	if env.Command != "checkpoint" {
		t.Errorf("envelope command = %q, want \"checkpoint\"", env.Command)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	// JSON must not carry human relative-time phrases.
	for _, banned := range []string{" ago", "just now"} {
		if strings.Contains(stdout, banned) {
			t.Errorf("checkpoint --json contains human relative time %q:\n%s", banned, stdout)
		}
	}
}

// TestLogAndStatusUseCheckpointVocabulary pins that the review surfaces name the
// object consistently: the empty log says "no checkpoints yet", and the status
// dashboard leads its dirty summary with a "checkpoint:" baseline.
func TestLogAndStatusUseCheckpointVocabulary(t *testing.T) {
	root := initProject(t)

	// Empty log names checkpoints.
	if code, stdout, _ := awa(t, root, "log"); code != 0 || !strings.Contains(stdout, "no checkpoints yet") {
		t.Errorf("empty log = %q, want 'no checkpoints yet'", stdout)
	}

	write(t, root, "calc.go", "package calc\n")
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "status baseline"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, _ := awa(t, root, "status")
	if code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	for _, want := range []string{"checkpoints:", "checkpoint:  checkpoint "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status missing %q:\n%s", want, stdout)
		}
	}
}
