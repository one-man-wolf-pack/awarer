package runevidence_test

import (
	"testing"

	"awarer/internal/domain/runevidence"
)

// Mutation proofs:
//   - drop the presence.Valid() guard in NewOutputInspectability ->
//     TestNewOutputInspectabilityRejectsInvalidPresence goes red.
//   - make Integrity() return anything but IntegrityUnverified ->
//     TestOutputInspectabilityIntegrityIsAlwaysUnverified goes red.

func TestPresenceStringAndValid(t *testing.T) {
	cases := map[runevidence.Presence]string{
		runevidence.PresencePresent:    "present",
		runevidence.PresenceMissing:    "missing",
		runevidence.PresenceUnreadable: "unreadable",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Presence(%d).String() = %q, want %q", int(p), got, want)
		}
		if !p.Valid() {
			t.Errorf("Presence %q must be valid", want)
		}
	}
	if (runevidence.Presence(99)).Valid() {
		t.Error("an out-of-range presence must not be valid")
	}
}

func TestNewOutputInspectabilityRejectsInvalidPresence(t *testing.T) {
	if _, err := runevidence.NewOutputInspectability(runevidence.Presence(99)); err == nil {
		t.Error("NewOutputInspectability must reject an out-of-range presence")
	}
	for _, p := range []runevidence.Presence{runevidence.PresencePresent, runevidence.PresenceMissing, runevidence.PresenceUnreadable} {
		if _, err := runevidence.NewOutputInspectability(p); err != nil {
			t.Errorf("NewOutputInspectability(%s) rejected: %v", p, err)
		}
	}
}

func TestOutputInspectabilityIntegrityIsAlwaysUnverified(t *testing.T) {
	// Metadata inspection never reads payload bytes, so integrity is always unverified —
	// a successful presence check is never proof the bytes are intact.
	for _, p := range []runevidence.Presence{runevidence.PresencePresent, runevidence.PresenceMissing, runevidence.PresenceUnreadable} {
		o, err := runevidence.NewOutputInspectability(p)
		if err != nil {
			t.Fatalf("NewOutputInspectability(%s): %v", p, err)
		}
		if o.Presence() != p {
			t.Errorf("Presence() = %s, want %s", o.Presence(), p)
		}
		if o.Integrity() != runevidence.IntegrityUnverified {
			t.Errorf("Integrity() = %s, want unverified", o.Integrity())
		}
		if o.Integrity().String() != "unverified" {
			t.Errorf("Integrity().String() = %q, want unverified", o.Integrity().String())
		}
	}
}
