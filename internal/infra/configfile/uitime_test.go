package configfile

import (
	"testing"

	"awarer/internal/domain/config"
)

func TestDecodeUITime(t *testing.T) {
	cfg, err := Decode([]byte("[ui]\ntime = \"utc\"\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.UI.Time != config.TimeUTC {
		t.Errorf("ui.time = %v, want utc", cfg.UI.Time)
	}
}

func TestDecodeUITimeDefaultsRelative(t *testing.T) {
	// An empty config keeps the relative default.
	cfg, err := Decode(nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.UI.Time != config.TimeRelative {
		t.Errorf("ui.time default = %v, want relative", cfg.UI.Time)
	}
}

func TestDecodeUITimeRejectsUnknown(t *testing.T) {
	if _, err := Decode([]byte("[ui]\ntime = \"yesterday\"\n")); err == nil {
		t.Error("an unknown ui.time value must be rejected")
	}
	// An unknown key under [ui] is rejected (typo detection).
	if _, err := Decode([]byte("[ui]\ntiem = \"utc\"\n")); err == nil { //nolint:misspell // deliberate typo: exercises unknown-key rejection
		t.Error("an unknown [ui] key must be rejected")
	}
}
