package help

import (
	"strings"
	"testing"

	"awarer/internal/domain/docbundle"
	"awarer/internal/domain/perfdiag"
)

// This file guards the two facts about authored topic CONTENT that a machine owns: the
// upper bound on a page's size, and the requirement that a closed token vocabulary the
// binary prints is a vocabulary the corpus explains. Catalog identity and Markdown
// mechanics live in help_test.go.
//
// Wording, section order, layout, and page organization are not checked here. They are
// authoring and review concerns, and the corpus itself is their artifact.
//
// Only authored topics are checked. Generated documents (the configuration reference)
// are projections of a live model; their shape is that model's business.

// authoredTopics returns the topics whose body a human wrote.
func authoredTopics(t *testing.T) []Topic {
	t.Helper()
	var out []Topic
	for _, topic := range Topics() {
		if topic.Kind == docbundle.KindTopic {
			out = append(out, topic)
		}
	}
	// Anti-vacuity: an empty selection means the filter, not the corpus, changed.
	if len(out) == 0 {
		t.Fatal("no authored topics found; the selection looks wrong")
	}
	return out
}

// maxTopicBytes is the ceiling on one authored topic. Past this size a page is
// answering several questions and should be split or should delegate through a link.
// There is deliberately no floor: whether a page says enough is a review judgement,
// not a byte count.
const maxTopicBytes = 12000

func TestEveryTopicStaysWithinItsByteCeiling(t *testing.T) {
	for _, topic := range authoredTopics(t) {
		if n := len(topic.Body); n > maxTopicBytes {
			t.Errorf("topic %q is %d bytes, above the %d-byte ceiling: split it or delegate through links",
				topic.Slug, n, maxTopicBytes)
		}
	}
}

// latencyTopics are the two topics that jointly own the latency vocabulary: `run`
// describes a diagnostic as part of the run contract, `troubleshooting` as the answer
// to "why is this slow". Both must carry every token, so adding a cause to one and not
// the other is caught rather than averaged away.
var latencyTopics = []string{"run", "troubleshooting"}

// TestLatencyVocabularyIsDocumented proves the tokens awa prints are the tokens awa
// explains.
//
// The binary is the only published source an agent has: it cannot read the repository,
// and a token that reaches stdout without reaching the help corpus is a word the
// consumer must guess the meaning of.
//
// Every expectation is enumerated from perfdiag, which is the whole point of the check.
// A hand-written list can only fail for the tokens someone remembered to add to it, so
// it would go green for exactly the change it exists to catch: a new cause minted in the
// domain, emitted into JSON, and never documented.
func TestLatencyVocabularyIsDocumented(t *testing.T) {
	var tokens []string
	for _, c := range perfdiag.Causes() {
		tokens = append(tokens, c.String())
	}
	for _, s := range perfdiag.Stages() {
		tokens = append(tokens, s.String())
	}
	for _, k := range perfdiag.HintKinds() {
		tokens = append(tokens, k.String())
	}
	// Anti-vacuity: an enumerator that returned nothing would make this pass silently.
	if len(tokens) == 0 {
		t.Fatal("no latency tokens enumerated; the catalogs look wrong")
	}

	for _, name := range latencyTopics {
		t.Run(name, func(t *testing.T) {
			topic, ok := Resolve(name)
			if !ok {
				t.Fatalf("topic %q does not resolve", name)
			}
			// The body is read raw. Every token here is a wire spelling with no
			// whitespace in it, so no rewrapping of the surrounding prose can split
			// one, and normalizing the page first would protect against nothing.
			for _, token := range tokens {
				if !strings.Contains(topic.Body, token) {
					t.Errorf("topic %q does not document %q", name, token)
				}
			}
		})
	}
}
