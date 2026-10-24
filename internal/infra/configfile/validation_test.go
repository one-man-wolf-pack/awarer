package configfile

import (
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
)

const sharedValidation = `[[run.validation]]
name = "cargo-test"
argv = ["cargo", "test"]
cwd = "."
unobserved_dirs = ["target"]
`

func TestComposeDecodesValidationPolicies(t *testing.T) {
	cfg, origins, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: overlay(t, sharedValidation)}})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	want := config.ValidationPolicy{Name: "cargo-test", Argv: []string{"cargo", "test"}, CWD: ".", UnobservedDirs: []string{"target"}}
	if len(cfg.Run.Validation) != 1 {
		t.Fatalf("validation = %+v, want one policy", cfg.Run.Validation)
	}
	got := cfg.Run.Validation[0]
	if got.Name != want.Name || got.CWD != want.CWD || !slices.Equal(got.Argv, want.Argv) || !slices.Equal(got.UnobservedDirs, want.UnobservedDirs) {
		t.Fatalf("validation[0] = %+v, want %+v", got, want)
	}
	if origins["run.validation"] != config.LayerShared {
		t.Errorf("run.validation origin = %q, want shared", origins["run.validation"])
	}
}

func TestComposeValidationListComposition(t *testing.T) {
	local := `[[run.validation]]
name = "go-test"
argv = ["go", "test", "./..."]
cwd = "."
unobserved_dirs = ["bin"]
`
	cases := []struct {
		name   string
		local  string
		want   []string
		origin config.Layer
	}{
		{"absent keeps the shared list", "[run]\nttl = \"1d\"\n", []string{"cargo-test"}, config.LayerShared},
		{"empty list clears it", "[run]\nvalidation = []\n", nil, config.LayerLocal},
		{"a list replaces it whole", local, []string{"go-test"}, config.LayerLocal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, origins, err := Compose([]NamedOverlay{
				{Layer: config.LayerShared, Overlay: overlay(t, sharedValidation)},
				{Layer: config.LayerLocal, Overlay: overlay(t, tc.local)},
			})
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			var names []string
			for _, p := range cfg.Run.Validation {
				names = append(names, p.Name)
			}
			if !slices.Equal(names, tc.want) {
				t.Errorf("policies = %v, want %v", names, tc.want)
			}
			if origins["run.validation"] != tc.origin {
				t.Errorf("origin = %q, want %q", origins["run.validation"], tc.origin)
			}
		})
	}
}

func TestComposeRejectsInvalidValidationPolicies(t *testing.T) {
	if _, err := DecodeOverlay([]byte(sharedValidation + "required_probe = \"x\"\n")); err == nil {
		t.Error("an unknown key inside [[run.validation]] was accepted")
	}
	dup := sharedValidation + strings.Replace(sharedValidation, `"cargo-test"`, `"again"`, 1)
	_, _, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: overlay(t, dup)}})
	if err == nil || !strings.Contains(err.Error(), "conflicts with run.validation[0]") {
		t.Fatalf("Compose = %v, want a duplicate-selector conflict", err)
	}
}
