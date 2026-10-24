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
	if cfg.Run.AllowIdentityProbes {
		t.Fatal("declaring a probe granted permission to execute it")
	}
}

func TestIdentityProbeGrantIsAcceptedOnlyFromThePrivateLayer(t *testing.T) {
	grant := "[run]\nallow_identity_probes = true\n"
	for _, layer := range []config.Layer{config.LayerShared, config.LayerConfig} {
		t.Run(layer.String(), func(t *testing.T) {
			_, _, err := Compose([]NamedOverlay{
				{Layer: config.LayerLocal, Overlay: overlay(t, "[run]\nallow_identity_probes = false\n")},
				{Layer: layer, Path: "/p/file.toml", Overlay: overlay(t, grant)},
			})
			if err == nil || !strings.Contains(err.Error(), "run.allow_identity_probes") || !strings.Contains(err.Error(), ".awa/config.toml") {
				t.Fatalf("Compose = %v, want a refusal naming the private config", err)
			}
		})
	}
	cfg, origins, err := Compose([]NamedOverlay{
		{Layer: config.LayerShared, Overlay: overlay(t, sharedProbes)},
		{Layer: config.LayerLocal, Overlay: overlay(t, grant)},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if !cfg.Run.AllowIdentityProbes || origins["run.allow_identity_probes"] != config.LayerLocal {
		t.Fatalf("grant = %t from %s, want true from local", cfg.Run.AllowIdentityProbes, origins["run.allow_identity_probes"])
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
