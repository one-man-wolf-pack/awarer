package checkpointjson

import (
	"testing"
)

// TestDecodeStrictJSONRejectsUnknownAndTrailing pins the shared strict reader: a
// well-formed document decodes, but an unknown field or trailing bytes is rejected,
// so every persisted record path inherits the exact-schema-shape guarantee from one
// place.
func TestDecodeStrictJSONRejectsUnknownAndTrailing(t *testing.T) {
	type doc struct {
		A int `json:"a"`
	}
	var d doc
	if err := decodeStrictJSON([]byte(`{"a":1}`), &d); err != nil || d.A != 1 {
		t.Fatalf("clean decode: err=%v a=%d, want a=1", err, d.A)
	}
	if err := decodeStrictJSON([]byte(`{"a":1,"b":2}`), &d); err == nil {
		t.Error("unknown field accepted, want rejection")
	}
	if err := decodeStrictJSON([]byte(`{"a":1}{"a":2}`), &d); err == nil {
		t.Error("trailing document accepted, want rejection")
	}
	if err := decodeStrictJSON([]byte(`{"a":1} garbage`), &d); err == nil {
		t.Error("trailing bytes accepted, want rejection")
	}
}
