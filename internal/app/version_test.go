package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"awarer/internal/output"
)

func TestVersion_Human(t *testing.T) {
	// Arrange
	var out, errBuf bytes.Buffer
	w := output.New(&out, &errBuf)

	// Act
	if err := Version(w, false); err != nil {
		t.Fatalf("Version returned error: %v", err)
	}

	// Assert: a single line naming awa and the version, nothing on stderr.
	got := out.String()
	if !strings.HasPrefix(got, "awa "+version) {
		t.Errorf("stdout = %q, want it to start with %q", got, "awa "+version)
	}
	if errBuf.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errBuf.String())
	}
}

func TestVersion_JSON(t *testing.T) {
	// Arrange
	var out, errBuf bytes.Buffer
	w := output.New(&out, &errBuf)

	// Act
	if err := Version(w, true); err != nil {
		t.Fatalf("Version returned error: %v", err)
	}

	// Assert: valid envelope carrying the version in data.
	var env struct {
		Command string    `json:"command"`
		Data    BuildInfo `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not valid json: %v\n%s", err, out.String())
	}
	if env.Command != "version" {
		t.Errorf("command = %q, want %q", env.Command, "version")
	}
	if env.Data.Version != version {
		t.Errorf("data.version = %q, want %q", env.Data.Version, version)
	}
}

func TestBuildInfo_Human(t *testing.T) {
	tests := []struct {
		name string
		bi   BuildInfo
		want string
	}{
		{
			name: "version only",
			bi:   BuildInfo{Version: "1.2.3"},
			want: "awa 1.2.3",
		},
		{
			name: "revision truncated and go appended",
			bi:   BuildInfo{Version: "1.2.3", Revision: "abcdef1234567890", Go: "go1.26.5"},
			want: "awa 1.2.3 (abcdef1, go1.26.5)",
		},
		{
			name: "go only",
			bi:   BuildInfo{Version: "1.2.3", Go: "go1.26.5"},
			want: "awa 1.2.3 (go1.26.5)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.bi.human(); got != tt.want {
				t.Errorf("human() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSourceCommitOnlyPublishesADescribingCommit pins the provenance rule the
// documentation export depends on: a commit is published only when the binary was
// built from exactly that commit. A binary built from a modified worktree carries
// the revision it branched from, which does not describe what it compiled, so the
// export must claim no commit at all rather than an inaccurate one. The dirty flag
// is a decision input, never an output.
func TestSourceCommitOnlyPublishesADescribingCommit(t *testing.T) {
	tests := []struct {
		name string
		bi   BuildInfo
		want string
	}{
		{
			name: "clean checkout publishes the commit",
			bi:   BuildInfo{Version: "1.2.3", Revision: "abcdef1234567890"},
			want: "abcdef1234567890",
		},
		{
			name: "modified worktree publishes no commit",
			bi:   BuildInfo{Version: "1.2.3", Revision: "abcdef1234567890", modified: true},
			want: "",
		},
		{
			name: "build outside a checkout publishes no commit",
			bi:   BuildInfo{Version: "1.2.3"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceCommit(tt.bi); got != tt.want {
				t.Errorf("sourceCommit() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildInfoNeverSerializesTheDirtyFlag proves the modified flag stays a
// decision input: mutable build provenance must not reach any output surface,
// including the version envelope.
func TestBuildInfoNeverSerializesTheDirtyFlag(t *testing.T) {
	raw, err := json.Marshal(BuildInfo{Version: "1.2.3", Revision: "abc1234", modified: true})
	if err != nil {
		t.Fatalf("marshal BuildInfo: %v", err)
	}
	for _, forbidden := range []string{"modified", "dirty", "true"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("BuildInfo JSON leaks the dirty flag (%q): %s", forbidden, raw)
		}
	}
}
