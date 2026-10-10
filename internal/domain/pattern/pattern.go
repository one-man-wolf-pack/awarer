package pattern

import (
	"fmt"
	"strings"
)

// Generation identifies the matching semantics behind every scan and retention
// decision. Identical pattern text selects different paths under different
// semantics, so each policy identity that covers pattern text also covers this
// value; raise it with any change to what a pattern matches, or results recorded
// under the old meaning are reused as if they agreed.
const Generation = 2

type Origin struct {
	File  string
	Line  int
	Key   string
	Index int
}

func FileLine(file string, line int) Origin { return Origin{File: file, Line: line} }

func KeyElement(key string, index int) Origin { return Origin{Key: key, Index: index} }

func (o Origin) InFile() bool { return o.File != "" }

func (o Origin) String() string {
	if o.InFile() {
		return fmt.Sprintf("%s:%d", o.File, o.Line)
	}
	return fmt.Sprintf("%s[%d]", o.Key, o.Index)
}

type Rule struct {
	// Text is one framed source element: an ignore-file line without its line
	// ending, or one decoded configuration value.
	Text string
	// Base is the declaring directory as literal path data ("" for the project
	// root). Joining it into Text would turn its glob characters into syntax.
	Base   string
	Origin Origin
}

func KeyRules(key string, elements []string) []Rule {
	rules := make([]Rule, len(elements))
	for i, text := range elements {
		rules[i] = Rule{Text: text, Origin: KeyElement(key, i)}
	}
	return rules
}

type Diagnostic struct {
	Origin Origin
	Text   string
	Reason string
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: pattern %q never matches: %s", d.Origin, d.Text, d.Reason)
}

// CarrierProblem returns "" for a configuration value that can carry one pattern.
// Lists published before pattern diagnostics keep their carriage returns, which
// those releases accepted; strict is for a list no published release has read.
func CarrierProblem(text string, strict bool) string {
	if strings.Contains(text, "\n") {
		return "must not contain a line feed: each element is one pattern"
	}
	if strict && strings.Contains(text, "\r") {
		return "must not contain a carriage return"
	}
	return ""
}
