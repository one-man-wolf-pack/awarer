package configfile

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
)

const sharedRequiredEnv = `[[run.required_env]]
argv = ["dotnet", "restore"]
cwd = "."
names = ["NUGET_PACKAGES"]
`

func TestComposeRequiredEnvLayersWholeListsWithOrigins(t *testing.T) {
	local := `[[run.required_env]]
argv = ["go", "test", "./..."]
cwd = "svc"
names = ["GOFLAGS", "GOPROXY"]
`
	cases := []struct {
		name   string
		local  string
		want   [][]string
		origin config.Layer
	}{
		{"absent keeps the shared list", "[run]\nttl = \"1d\"\n", [][]string{{"dotnet", "restore"}}, config.LayerShared},
		{"empty list clears it", "[run]\nrequired_env = []\n", nil, config.LayerLocal},
		{"a list replaces it whole", local, [][]string{{"go", "test", "./..."}}, config.LayerLocal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, origins, err := Compose([]NamedOverlay{
				{Layer: config.LayerShared, Overlay: overlay(t, sharedRequiredEnv)},
				{Layer: config.LayerLocal, Overlay: overlay(t, tc.local)},
			})
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			var argvs [][]string
			for _, d := range cfg.Run.RequiredEnv {
				argvs = append(argvs, d.Argv)
			}
			if !slices.EqualFunc(argvs, tc.want, slices.Equal) {
				t.Errorf("declarations = %q, want %q", argvs, tc.want)
			}
			if origins["run.required_env"] != tc.origin {
				t.Errorf("origin = %q, want %q", origins["run.required_env"], tc.origin)
			}
		})
	}
}

func TestComposeRequiredEnvRejectsAmbiguousAndUnknownFields(t *testing.T) {
	_, _, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: overlay(t, sharedRequiredEnv+"\n"+sharedRequiredEnv)}})
	var verr *config.ValidationError
	if err == nil || !strings.Contains(err.Error(), "conflicts with run.required_env[0]") {
		t.Fatalf("Compose = %v, want a duplicate-selector error", err)
	}
	if !errors.As(err, &verr) {
		t.Fatalf("duplicate selector is %T, want a config validation error", err)
	}
	if _, err := DecodeOverlay([]byte("[[run.required_env]]\nargv = [\"x\"]\ncwd = \".\"\nnames = [\"A\"]\nvalues = [\"secret\"]\n")); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("DecodeOverlay = %v, want an unknown-key error", err)
	}
}
