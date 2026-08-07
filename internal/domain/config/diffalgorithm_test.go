package config_test

import (
	"testing"

	"awarer/internal/domain/config"
)

func TestParseDiffAlgorithm(t *testing.T) {
	cases := []struct {
		in      string
		want    config.DiffAlgorithm
		wantErr bool
	}{
		{"myers", config.DiffMyers, false},
		{"histogram", config.DiffHistogram, false},
		{"", 0, true},
		{"Myers", 0, true},
		{"histogramm", 0, true},
		{"bogus", 0, true},
	}
	for _, c := range cases {
		got, err := config.ParseDiffAlgorithm(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDiffAlgorithm(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDiffAlgorithm(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDiffAlgorithm(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDiffAlgorithmDefaultIsHistogram(t *testing.T) {
	if got := config.DefaultDiffAlgorithm(); got != config.DiffHistogram {
		t.Errorf("DefaultDiffAlgorithm() = %v, want histogram", got)
	}
	if got := config.Defaults().Diff.Algorithm; got != config.DiffHistogram {
		t.Errorf("Defaults().Diff.Algorithm = %v, want histogram", got)
	}
}

func TestDiffAlgorithmStringRoundTrip(t *testing.T) {
	for _, a := range []config.DiffAlgorithm{config.DiffMyers, config.DiffHistogram} {
		got, err := config.ParseDiffAlgorithm(a.String())
		if err != nil || got != a {
			t.Errorf("round-trip %v: String()=%q parse=%v err=%v", a, a.String(), got, err)
		}
	}
}

func TestDiffAlgorithmValidation(t *testing.T) {
	if !config.DiffMyers.Valid() || !config.DiffHistogram.Valid() {
		t.Error("known algorithms must be Valid")
	}
	if config.DiffAlgorithm(99).Valid() {
		t.Error("unknown algorithm must not be Valid")
	}
	// An out-of-range enum must fail Config.Validate.
	c := config.Defaults()
	c.Diff.Algorithm = config.DiffAlgorithm(99)
	if err := c.Validate(); err == nil {
		t.Error("Validate must reject an unknown diff algorithm")
	}
}
