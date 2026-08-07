package main

import (
	"strings"
	"testing"
)

func TestParseBaseURLAcceptsARootOrigin(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://awarer.one-man-wolf-pack.com", "https://awarer.one-man-wolf-pack.com"},
		{"https://awarer.one-man-wolf-pack.com/", "https://awarer.one-man-wolf-pack.com"},
		{"https://localhost:8443", "https://localhost:8443"},
	}
	for _, tc := range tests {
		b, err := ParseBaseURL(tc.raw)
		if err != nil {
			t.Fatalf("ParseBaseURL(%q): %v", tc.raw, err)
		}
		if b.String() != tc.want {
			t.Errorf("ParseBaseURL(%q) = %q, want %q", tc.raw, b.String(), tc.want)
		}
	}
}

func TestParseBaseURLRefusesEverythingElse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", "required"},
		{"http", "http://awarer.example", "must use https"},
		{"no host", "https://", "no host"},
		{"credentials", "https://user:pw@awarer.example", "credentials"},
		{"query", "https://awarer.example/?utm=1", "query"},
		{"fragment", "https://awarer.example/#top", "fragment"},
		{"sub-path", "https://awarer.example/docs", "sub-path"},
		{"opaque", "https:awarer.example", "hierarchical"},
		{"not a url", "https://awarer example", "not a url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBaseURL(tc.raw)
			if err == nil {
				t.Fatalf("ParseBaseURL accepted %q", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestBaseURLAbsoluteJoinsWithoutADoubleSlash(t *testing.T) {
	b, err := ParseBaseURL("https://awarer.example/")
	if err != nil {
		t.Fatalf("ParseBaseURL: %v", err)
	}
	tests := map[string]string{
		"/":             "https://awarer.example/",
		"/docs/":        "https://awarer.example/docs/",
		"/docs/agents/": "https://awarer.example/docs/agents/",
		"/robots.txt":   "https://awarer.example/robots.txt",
	}
	for route, want := range tests {
		if got := b.Absolute(route); got != want {
			t.Errorf("Absolute(%q) = %q, want %q", route, got, want)
		}
	}
}
