package checkpoint

import "testing"

func TestParseCheckpointMessage(t *testing.T) {
	empty, err := ParseCheckpointMessage("")
	if err != nil || !empty.IsZero() {
		t.Fatalf("empty message: %v, zero=%v", err, empty.IsZero())
	}
	m, err := ParseCheckpointMessage("before parser rewrite")
	if err != nil {
		t.Fatalf("ParseCheckpointMessage: %v", err)
	}
	if m.String() != "before parser rewrite" {
		t.Fatalf("String() = %q", m.String())
	}
	if _, err := ParseCheckpointMessage("has\x00nul"); err == nil {
		t.Fatal("expected error for NUL byte")
	}
}

func TestCheckpointMessageFirstLine(t *testing.T) {
	m, _ := ParseCheckpointMessage("first line\nsecond line")
	if m.FirstLine() != "first line" {
		t.Fatalf("FirstLine() = %q", m.FirstLine())
	}
}
