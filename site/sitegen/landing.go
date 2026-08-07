package main

import (
	_ "embed"
	"fmt"
	"strings"

	"awarer/internal/domain/docbundle"
)

// The landing page is the only place on this site where words did not come out
// of the binary. It therefore says as little as it can get away with: what the
// tool is, one real workflow, and a dense set of pointers into the exported
// documentation. It deliberately does not restate syntax, defaults, safety
// boundaries, or exit codes — those belong to the documents, and a second copy
// here would be a second contract to keep in step.
//
// The copy is a typed literal rather than a Markdown file. Prose fields carry
// inline Markdown (so `awa status` reads as code) and go through the same
// hardened renderer as the corpus, with every link form refused; every link on
// the page is a typed field the template renders. That means a landing edit can
// change wording but cannot introduce markup, a foreign origin, or a pointer to
// a document that does not exist.

// sourceURL and releasesURL are the two identity URLs the product documents, and
// the only external destinations the site links to. Both are composed from one
// prefix rather than spelled out, so neither can be edited to point at a
// neighbouring repository without moving the product's own address with it.
// What the built pages actually carry is asserted over the rendered bytes.
const (
	allowedLinkPrefix = "https://github.com/one-man-wolf-pack/awarer"
	sourceURL         = allowedLinkPrefix
	releasesURL       = allowedLinkPrefix + "/releases"
)

//go:embed fixtures/terminal-status.txt
var terminalFixture string

// landingCard is one question a reader arrives with, and the exported document
// that answers it. Slug is resolved against the manifest, so a card pointing at
// a document this release does not carry fails the build instead of publishing a
// dead link.
type landingCard struct {
	question string
	answer   string
	slug     string
}

// landingAction is a button on the landing page. It names a document by slug
// rather than by URL, so the route is built in one place and a slug this release
// does not carry fails the build instead of publishing a dead button. An empty
// slug means the documentation index, which is a site route rather than a
// document.
type landingAction struct {
	label   string
	slug    string
	primary bool
}

// landingContent is the whole hand-authored surface of the site.
type landingContent struct {
	headline        string
	subhead         string
	heroActions     []landingAction
	workflowCaption string
	workflow        []workflowStep
	cards           []landingCard
	terminalCaption string
	installNote     string
	installActions  []landingAction
}

var landingCopy = landingContent{
	headline: "`awa` gives an agent a memory of what it changed.",
	subhead: "Explicit worktree checkpoints, reusable deterministic checks, and durable " +
		"execution history — one local binary, no server, no account.",

	heroActions: []landingAction{
		{label: "Install", slug: "install", primary: true},
		{label: "Agent quick path", slug: "agents"},
		{label: "All documentation"},
	},

	workflowCaption: "Inside an initialized project, this is the whole loop. Built-in defaults are enough.",
	workflow: []workflowStep{
		{Command: "awa status", Comment: "the review dashboard: baseline, dirt, reusable checks, next steps"},
		{Command: `awa checkpoint -m "before extracting the rate limiter"`, Comment: "mark the worktree before a meaningful change"},
		{Command: "awa run -- go test ./...", Comment: "run a check; an unchanged rerun is served from cache"},
		{Command: "awa changes", Comment: "what moved since that checkpoint"},
		{Command: "awa diff src/", Comment: "read the delta, narrowed to a path"},
	},

	cards: []landingCard{
		{
			question: "I am an agent. What is the loop?",
			answer:   "The four commands that cover checkpoints, cached checks, supervised records, and output inspection.",
			slug:     "agents",
		},
		{
			question: "How do I install it?",
			answer:   "Release archives, checksum verification, and what upgrading changes.",
			slug:     "install",
		},
		{
			question: "How do I review what changed?",
			answer:   "Ranges, baselines, and why a clean delta is not the same as a reviewed worktree.",
			slug:     "diff",
		},
		{
			question: "Why is my check no longer reusable?",
			answer:   "Near misses, mismatch reasons, and what the run cache does and does not observe.",
			slug:     "run",
		},
		{
			question: "What does `awa` store on my disk?",
			answer:   "Everything `.awa/` keeps, why it is owner-private, and what must never be committed.",
			slug:     "privacy",
		},
		{
			question: "How do I parse the output?",
			answer:   "The schema-versioned JSON envelope, and how to tell a failed command from a broken protocol.",
			slug:     "json",
		},
		{
			question: "Something is wrong. Where do I start?",
			answer:   "Diagnosis by symptom, the `awa doctor` finding codes, and how to recover state.",
			slug:     "troubleshooting",
		},
		{
			question: "Where is every command and flag?",
			answer:   "The exhaustive syntax reference, generated from the binary's own command registry.",
			slug:     "commands",
		},
	},

	terminalCaption: "Recorded output from a scratch project — an example, not live state.",
	installNote:     "Prebuilt archives are published for Linux, macOS, FreeBSD, and Windows. Verify the checksum before you run it.",
	installActions: []landingAction{
		{label: "Installation guide", slug: "install", primary: true},
	},
}

// buildLanding turns the authored copy into a page model, resolving every card
// against the export and rendering every prose field through the hardened
// inline renderer.
func buildLanding(b *Bundle) (*landingView, error) {
	headline, err := renderProse("landing headline", landingCopy.headline)
	if err != nil {
		return nil, err
	}
	subhead, err := renderProse("landing subhead", landingCopy.subhead)
	if err != nil {
		return nil, err
	}
	workflowCaption, err := renderProse("landing workflow caption", landingCopy.workflowCaption)
	if err != nil {
		return nil, err
	}
	installNote, err := renderProse("landing install note", landingCopy.installNote)
	if err != nil {
		return nil, err
	}

	bySlug := make(map[string]docbundle.ManifestEntry, len(b.Documents()))
	for _, e := range b.Documents() {
		bySlug[e.Slug().String()] = e
	}

	heroActions, err := resolveActions(bySlug, "hero", landingCopy.heroActions)
	if err != nil {
		return nil, err
	}
	installActions, err := resolveActions(bySlug, "install section", landingCopy.installActions)
	if err != nil {
		return nil, err
	}

	cards := make([]cardView, 0, len(landingCopy.cards))
	seen := make(map[string]bool, len(landingCopy.cards))
	for _, c := range landingCopy.cards {
		entry, ok := bySlug[c.slug]
		if !ok {
			return nil, fmt.Errorf("landing card %q points at document %q, which this release does not carry", c.question, c.slug)
		}
		if seen[c.slug] {
			return nil, fmt.Errorf("landing lists document %q twice", c.slug)
		}
		seen[c.slug] = true
		answer, err := renderProse("landing card "+c.slug, c.answer)
		if err != nil {
			return nil, err
		}
		cards = append(cards, cardView{
			Question: c.question,
			Answer:   answer,
			Route:    docRoute(entry.Slug()),
			DocTitle: entry.Title(),
		})
	}

	terminal, err := buildTerminal()
	if err != nil {
		return nil, err
	}

	return &landingView{
		Headline:    headline,
		Subhead:     subhead,
		HeroActions: heroActions,
		Workflow: workflowView{
			Caption: workflowCaption,
			Steps:   landingCopy.workflow,
		},
		Cards:          cards,
		Terminal:       terminal,
		InstallNote:    installNote,
		InstallActions: installActions,
	}, nil
}

// resolveActions turns authored buttons into published ones, building every
// route through docRoute so the landing cannot invent a URL shape of its own.
func resolveActions(bySlug map[string]docbundle.ManifestEntry, where string, actions []landingAction) ([]actionView, error) {
	out := make([]actionView, 0, len(actions))
	for _, a := range actions {
		if a.slug == "" {
			out = append(out, actionView{Label: a.label, Route: routeDocs, Primary: a.primary})
			continue
		}
		entry, ok := bySlug[a.slug]
		if !ok {
			return nil, fmt.Errorf("landing %s button %q points at document %q, which this release does not carry", where, a.label, a.slug)
		}
		out = append(out, actionView{Label: a.label, Route: docRoute(entry.Slug()), Primary: a.primary})
	}
	return out, nil
}

// leakedPathMarkers are spellings that would mean the recorded terminal block
// carries the machine it was captured on rather than an example. The fixture is
// reviewed by hand; this is the check that keeps a later re-recording from
// publishing a home directory.
var leakedPathMarkers = []string{"/Users/", "/root/", "/private/", "/var/folders/", `C:\`, "$HOME", "~/"}

// exampleRoot is the one absolute path the recording is allowed to show. The
// capture is normalised to it by hand; anything else under /home/ would be a
// real directory that survived the normalisation.
const exampleRoot = "/home/dev/"

// buildTerminal validates and splits the recorded terminal fixture.
func buildTerminal() (terminalView, error) {
	if terminalFixture == "" {
		return terminalView{}, fmt.Errorf("the terminal fixture is empty")
	}
	for _, marker := range leakedPathMarkers {
		if strings.Contains(terminalFixture, marker) {
			return terminalView{}, fmt.Errorf("the terminal fixture carries %q, which would publish a real path", marker)
		}
	}

	lines := strings.Split(strings.TrimSuffix(terminalFixture, "\n"), "\n")
	for _, line := range lines {
		if i := strings.Index(line, "/home/"); i >= 0 && !strings.HasPrefix(line[i:], exampleRoot) {
			return terminalView{}, fmt.Errorf("the terminal fixture carries the path %q, which is not the reviewed example root", strings.TrimSpace(line[i:]))
		}
		if strings.Contains(line, "\t") {
			return terminalView{}, fmt.Errorf("the terminal fixture carries a tab, which does not survive HTML layout")
		}
	}
	return terminalView{Caption: landingCopy.terminalCaption, Lines: lines}, nil
}
