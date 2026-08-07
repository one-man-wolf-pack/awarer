package worktree

import (
	"reflect"
	"testing"
)

func TestStatFieldTokenRoundTrip(t *testing.T) {
	for _, f := range []StatField{FieldCtime, FieldDev, FieldIno, FieldNlink} {
		got, err := ParseStatField(f.String())
		if err != nil {
			t.Fatalf("ParseStatField(%q): %v", f.String(), err)
		}
		if got != f {
			t.Fatalf("round trip changed field: %v -> %v", f, got)
		}
	}
	if _, err := ParseStatField("bogus"); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

func TestFieldSetTokens(t *testing.T) {
	set := FieldSet(0).With(FieldDev).With(FieldCtime)
	got := set.Tokens()
	want := []string{"ctime", "dev"} // stable order regardless of insertion
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokens() = %v, want %v", got, want)
	}
	if empty := FieldSet(0).Tokens(); len(empty) != 0 {
		t.Fatalf("empty set tokens = %v, want []", empty)
	}
}

func TestParseFieldSet(t *testing.T) {
	set, err := ParseFieldSet([]string{"nlink", "ctime"})
	if err != nil {
		t.Fatalf("ParseFieldSet: %v", err)
	}
	if !set.Has(FieldNlink) || !set.Has(FieldCtime) || set.Has(FieldDev) {
		t.Fatalf("unexpected set %b", set)
	}
	if _, err := ParseFieldSet([]string{"dev", "dev"}); err == nil {
		t.Fatal("expected error for duplicate token")
	}
	if _, err := ParseFieldSet([]string{"bogus"}); err == nil {
		t.Fatal("expected error for unknown token")
	}
}
