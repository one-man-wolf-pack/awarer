package cli

import (
	"testing"

	domainconfig "awarer/internal/domain/config"
)

// TestScopeIgnoredPathsOutsideEvidenceIsInvariant locks the contract assertion: the
// ignored_paths_outside_evidence field is always true, regardless of which ignore
// sources are configured — it is a stable statement that an awa delta says nothing about
// ignored paths, never a measured value that could be false.
func TestScopeIgnoredPathsOutsideEvidenceIsInvariant(t *testing.T) {
	cases := []struct {
		name string
		cfg  domainconfig.Config
	}{
		{"defaults", domainconfig.Defaults()},
		{"no ignore sources", domainconfig.Config{}},
		{
			"all ignore sources",
			domainconfig.Config{Scope: domainconfig.Scope{
				UseGitignore:  true,
				UseAwaignore:  true,
				ExtraExcludes: []string{"vendor"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := scopeViewOf(0, tc.cfg)
			if !v.IgnoredPathsOutsideEvidence {
				t.Errorf("ignored_paths_outside_evidence must be invariantly true, got false for %s", tc.name)
			}
		})
	}
}
