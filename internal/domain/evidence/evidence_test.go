package evidence

import "testing"

func TestComponentValidRejectsUnknownAndZero(t *testing.T) {
	// Arrange / Act / Assert: the zero value and an off-list string are not valid;
	// every declared component is.
	if (Component("")).Valid() {
		t.Error("zero Component is valid, want invalid")
	}
	if (Component("blobstore")).Valid() {
		t.Error("unknown Component is valid, want invalid")
	}
	for _, c := range []Component{
		ComponentCheckpoints, ComponentRuns, ComponentBlobs, ComponentIndex,
		ComponentLocks, ComponentConfig, ComponentStore,
	} {
		if !c.Valid() {
			t.Errorf("Component %q not valid", c)
		}
		if c.String() != string(c) {
			t.Errorf("Component.String() = %q, want %q", c.String(), string(c))
		}
	}
}

func TestDiagnosticTokenValidAndWire(t *testing.T) {
	if (DiagnosticToken("")).Valid() {
		t.Error("zero token is valid, want invalid")
	}
	if (DiagnosticToken("corrupt")).Valid() {
		t.Error("unknown token is valid, want invalid")
	}
	// Every declared token round-trips its wire string and is valid.
	for _, tok := range []DiagnosticToken{
		TokenStoreMissing, TokenStoreEmpty, TokenReadPartial, TokenReadBounded,
		TokenMetadataCorrupt, TokenMetadataIncompatible, TokenPayloadMissing,
		TokenPayloadCorrupt, TokenPermissionDenied, TokenIOError,
		TokenUnsupportedFormat, TokenUnknown,
	} {
		if !tok.Valid() {
			t.Errorf("token %q not valid", tok)
		}
		if tok.String() != string(tok) {
			t.Errorf("token.String() = %q, want %q", tok.String(), string(tok))
		}
	}
}

func TestBoundedCountKnownTotal(t *testing.T) {
	b := NewBoundedCount(16, 4000, 16)
	if b.Shown() != 16 {
		t.Errorf("Shown = %d, want 16", b.Shown())
	}
	total, ok := b.Total()
	if !ok || total != 4000 {
		t.Errorf("Total = (%d,%v), want (4000,true)", total, ok)
	}
	if b.Limit() != 16 {
		t.Errorf("Limit = %d, want 16", b.Limit())
	}
	if b.Complete() {
		t.Error("Complete = true, want false for a capped scan")
	}
}

func TestBoundedCountClampsShownToTotal(t *testing.T) {
	// A shown count above the total is impossible; the constructor clamps it so the
	// value can never claim to have read more than exists.
	b := NewBoundedCount(50, 10, 0)
	if b.Shown() != 10 {
		t.Errorf("Shown = %d, want clamped to 10", b.Shown())
	}
	if !b.Complete() {
		t.Error("Complete = false, want true when shown >= total")
	}
}

func TestBoundedCountUnknownTotalIsNeverComplete(t *testing.T) {
	b := NewBoundedCountUnknownTotal(16, 16)
	if _, ok := b.Total(); ok {
		t.Error("Total known, want unknown")
	}
	if b.Complete() {
		t.Error("Complete = true, want false when total is unknown")
	}
}

func TestNewDegradationRejectsInvalidAndBoundedToken(t *testing.T) {
	if _, ok := NewDegradation(Component("bogus"), TokenMetadataCorrupt, ""); ok {
		t.Error("built a degradation with an invalid component")
	}
	if _, ok := NewDegradation(ComponentRuns, DiagnosticToken("bogus"), ""); ok {
		t.Error("built a degradation with an invalid token")
	}
	if _, ok := NewDegradation(ComponentRuns, TokenReadBounded, ""); ok {
		t.Error("built a read-bounded degradation without a sample via NewDegradation")
	}
}

func TestNewDegradationAccessors(t *testing.T) {
	d, ok := NewDegradation(ComponentCheckpoints, TokenMetadataIncompatible, "incompatible schema")
	if !ok {
		t.Fatal("NewDegradation failed for valid input")
	}
	if d.Component() != ComponentCheckpoints {
		t.Errorf("Component = %q", d.Component())
	}
	if d.Token() != TokenMetadataIncompatible {
		t.Errorf("Token = %q", d.Token())
	}
	if d.Detail() != "incompatible schema" {
		t.Errorf("Detail = %q", d.Detail())
	}
	if _, ok := d.Sample(); ok {
		t.Error("non-bounded degradation carries a sample")
	}
}

func TestNewBoundedDegradationCarriesSample(t *testing.T) {
	sample := NewBoundedCount(16, 4000, 16)
	d, ok := NewBoundedDegradation(ComponentCheckpoints, sample, "checked newest 16")
	if !ok {
		t.Fatal("NewBoundedDegradation failed for valid input")
	}
	if d.Token() != TokenReadBounded {
		t.Errorf("Token = %q, want read-bounded", d.Token())
	}
	got, ok := d.Sample()
	if !ok {
		t.Fatal("bounded degradation carries no sample")
	}
	if got.Shown() != 16 {
		t.Errorf("sample Shown = %d, want 16", got.Shown())
	}
	if _, invalid := NewBoundedDegradation(Component("bogus"), sample, ""); invalid {
		t.Error("built a bounded degradation with an invalid component")
	}
}
