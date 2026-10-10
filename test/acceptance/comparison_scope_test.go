package acceptance

import (
	"encoding/json"
	"testing"
)

func TestComparisonScopeProvenanceAcrossPredecessorOperandKinds(t *testing.T) {
	root := predecessorProject(t)
	run := "run:" + predecessorRun
	cases := []struct {
		rng         string
		left, right string
	}{
		{predecessorCheckpoint + ".." + predecessorRepeat, "checkpoint", "checkpoint"},
		{predecessorCheckpoint + "..now", "checkpoint", "now"},
		{run + ":before.." + run + ":after", "run-observation", "run-observation"},
		{predecessorRecovery + "..now", "recovery-observation", "now"},
		{predecessorCheckpoint + ".." + predecessorRecovery, "checkpoint", "recovery-observation"},
	}
	for _, command := range []string{"changes", "diff"} {
		for _, tc := range cases {
			t.Run(command+" "+tc.rng, func(t *testing.T) {
				code, stdout, stderr := awa(t, root, "--json", command, tc.rng)
				if code != 0 {
					t.Fatalf("exit = %d, stderr = %q", code, stderr)
				}
				var env struct {
					Data struct {
						Left  struct{ Kind string } `json:"left"`
						Right struct{ Kind string } `json:"right"`
						Scope struct {
							IgnoreSources             []string          `json:"ignore_sources"`
							IgnoreSourcesBasis        string            `json:"ignore_sources_basis"`
							IgnoreSourceParticipation map[string]string `json:"ignore_source_participation"`
						} `json:"scope"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(stdout), &env); err != nil {
					t.Fatalf("decode: %v\n%s", err, stdout)
				}
				if env.Data.Left.Kind != tc.left || env.Data.Right.Kind != tc.right {
					t.Fatalf("operand kinds = %q..%q, want %q..%q", env.Data.Left.Kind, env.Data.Right.Kind, tc.left, tc.right)
				}
				s := env.Data.Scope
				if s.IgnoreSourcesBasis != "current-configuration" || len(s.IgnoreSources) == 0 {
					t.Errorf("scope sources = %v with basis %q, want current-configuration categories", s.IgnoreSources, s.IgnoreSourcesBasis)
				}
				if len(s.IgnoreSourceParticipation) != 2 || s.IgnoreSourceParticipation["left"] != "unknown" || s.IgnoreSourceParticipation["right"] != "unknown" {
					t.Errorf("ignore_source_participation = %v, want unknown on both sides", s.IgnoreSourceParticipation)
				}
			})
		}
	}
}
