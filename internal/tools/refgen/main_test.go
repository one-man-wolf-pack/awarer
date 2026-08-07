package main

import (
	"strings"
	"testing"
)

// TestRequireIdenticalRefusesAnEmptyDocument is the guard the old shell recipe
// needed a separate `[ -s ... ]` test for: two empty results compare equal, so a
// generator that succeeded and produced nothing would have made the whole reference
// gate pass without comparing anything.
func TestRequireIdenticalRefusesAnEmptyDocument(t *testing.T) {
	for _, tc := range []struct {
		name      string
		want, got []byte
	}{
		{"both empty", nil, nil},
		{"committed empty", nil, []byte("{}\n")},
		{"generated empty", []byte("{}\n"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireIdentical("subject", tc.want, tc.got)
			if err == nil {
				t.Fatal("an empty reference must be refused, not compared")
			}
			if !strings.Contains(err.Error(), "empty") {
				t.Errorf("error does not say the reference was empty: %v", err)
			}
		})
	}
}

func TestRequireIdenticalAcceptsEqualDocuments(t *testing.T) {
	doc := []byte("{\n  \"a\": 1\n}\n")
	if err := requireIdentical("subject", doc, append([]byte(nil), doc...)); err != nil {
		t.Fatalf("equal documents must compare equal: %v", err)
	}
}

// TestRequireIdenticalNamesTheDrift proves the failure is actionable: the subject
// and the first differing line, on both sides, reach the operator.
func TestRequireIdenticalNamesTheDrift(t *testing.T) {
	want := []byte("{\n  \"version\": 1,\n  \"commands\": []\n}\n")
	got := []byte("{\n  \"version\": 2,\n  \"commands\": []\n}\n")

	err := requireIdentical("drifted from testdata/reference.json", want, got)
	if err == nil {
		t.Fatal("differing documents must not compare equal")
	}
	msg := err.Error()
	for _, fragment := range []string{
		"drifted from testdata/reference.json",
		"line 2",
		`"version": 1`,
		`"version": 2`,
	} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("the report does not mention %q:\n%s", fragment, msg)
		}
	}
}

// TestRequireIdenticalReportsATruncatedDocument covers the shape a short write
// produces: a valid prefix and a missing tail.
func TestRequireIdenticalReportsATruncatedDocument(t *testing.T) {
	want := []byte("a\nb\nc")
	got := []byte("a\nb")

	err := requireIdentical("truncated", want, got)
	if err == nil {
		t.Fatal("a truncated document must not compare equal")
	}
	if !strings.Contains(err.Error(), "<end of file>") {
		t.Errorf("the report does not show where the generated document ended:\n%v", err)
	}
}

// TestGenerateIsDeterministic exercises the real generator through the same helper
// the -check mode uses, so a reference that reached the document through an
// unsorted map fails here rather than only in CI.
func TestGenerateIsDeterministic(t *testing.T) {
	first, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireIdentical("generator output is not deterministic", first, second); err != nil {
		t.Fatal(err)
	}
}
