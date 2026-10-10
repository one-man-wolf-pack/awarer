package cli

import (
	"encoding/json"
	"testing"

	domainconfig "awarer/internal/domain/config"
)

func TestScopeViewLabelsCurrentConfigurationAndUnknownParticipation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     domainconfig.Config
		skipped int
		want    string
	}{
		{
			"defaults", domainconfig.Defaults(), 0,
			`{"skipped_in_scope":0,"ignore_sources":["awaignore","baseline-excludes"],` +
				`"ignore_sources_basis":"current-configuration",` +
				`"ignore_source_participation":{"left":"unknown","right":"unknown"},` +
				`"ignored_paths_outside_evidence":true}`,
		},
		{
			"no ignore sources", domainconfig.Config{}, 3,
			`{"skipped_in_scope":3,"ignore_sources":["baseline-excludes"],` +
				`"ignore_sources_basis":"current-configuration",` +
				`"ignore_source_participation":{"left":"unknown","right":"unknown"},` +
				`"ignored_paths_outside_evidence":true}`,
		},
		{
			"all ignore sources",
			domainconfig.Config{Scope: domainconfig.Scope{
				UseGitignore:  true,
				UseAwaignore:  true,
				ExtraExcludes: []string{"vendor"},
			}},
			1,
			`{"skipped_in_scope":1,"ignore_sources":["gitignore","awaignore","config-excludes","baseline-excludes"],` +
				`"ignore_sources_basis":"current-configuration",` +
				`"ignore_source_participation":{"left":"unknown","right":"unknown"},` +
				`"ignored_paths_outside_evidence":true}`,
		},
		{
			"history excludes only",
			domainconfig.Config{History: domainconfig.History{ExtraExcludes: []string{"vendor"}}},
			0,
			`{"skipped_in_scope":0,"ignore_sources":["config-excludes","baseline-excludes"],` +
				`"ignore_sources_basis":"current-configuration",` +
				`"ignore_source_participation":{"left":"unknown","right":"unknown"},` +
				`"ignored_paths_outside_evidence":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(scopeViewOf(tc.skipped, tc.cfg))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("scope =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
