package configfile

import (
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
)

const sharedProbes = sharedValidation + `
[[run.validation.identity_probes]]
name = "rustc"
argv = ["rustc", "-vV"]
`

func TestComposeDecodesIdentityProbes(t *testing.T) {
	cfg, _, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: overlay(t, sharedProbes)}})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	probes := cfg.Run.Validation[0].IdentityProbes
	if len(probes) != 1 || probes[0].Name != "rustc" || !slices.Equal(probes[0].Argv, []string{"rustc", "-vV"}) {
		t.Fatalf("identity probes = %+v", probes)
	}
}

// No config file can grant probe execution: the former private-layer permission key is
// an unknown key everywhere, so a checkout carrying it cannot authorize itself.
func TestNoConfigLayerCanGrantIdentityProbes(t *testing.T) {
	_, err := DecodeOverlay([]byte("[run]\nallow_identity_probes = true\n"))
	if err == nil || !strings.Contains(err.Error(), "allow_identity_probes") {
		t.Fatalf("DecodeOverlay = %v, want an unknown-key refusal", err)
	}
}

func TestComposeRejectsInvalidIdentityProbes(t *testing.T) {
	probe := func(name, argv string) string {
		return "\n[[run.validation.identity_probes]]\nname = " + name + "\nargv = " + argv + "\n"
	}
	cases := map[string]string{
		"empty name":     probe(`""`, `["rustc", "-vV"]`),
		"empty argv":     probe(`"a"`, `[]`),
		"duplicate name": probe(`"a"`, `["x"]`) + probe(`"a"`, `["y"]`),
		"duplicate argv": probe(`"a"`, `["x", "1"]`) + probe(`"b"`, `["x", "1"]`),
	}
	var many string
	for i := range config.MaxIdentityProbes + 1 {
		many += probe(`"p`+string(rune('a'+i))+`"`, `["p`+string(rune('a'+i))+`"]`)
	}
	cases["too many"] = many
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: overlay(t, sharedValidation+extra)}})
			if err == nil || !strings.Contains(err.Error(), "identity_probes") {
				t.Fatalf("Compose = %v, want an identity_probes problem", err)
			}
		})
	}
}
