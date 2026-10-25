package runcache_test

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func TestProbeGrantContextIdentifiesWhatAGrantApproves(t *testing.T) {
	h := blake3hash.New()
	base := config.ValidationPolicy{
		Name: "check", Argv: []string{"cargo", "test"}, CWD: ".", UnobservedDirs: []string{"target"},
		IdentityProbes: []config.IdentityProbe{{Name: "rustc", Argv: []string{"rustc", "-vV"}}, {Name: "cargo", Argv: []string{"cargo", "-V"}}},
	}
	want := runcache.PolicyGrantContext(h, base)
	same := map[string]func(*config.ValidationPolicy){
		"policy label": func(p *config.ValidationPolicy) { p.Name = "other" },
		"probe label":  func(p *config.ValidationPolicy) { p.IdentityProbes[0].Name = "compiler" },
		"probe order": func(p *config.ValidationPolicy) {
			p.IdentityProbes[0], p.IdentityProbes[1] = p.IdentityProbes[1], p.IdentityProbes[0]
		},
		"unobserved dirs": func(p *config.ValidationPolicy) { p.UnobservedDirs = []string{"out"} },
	}
	differ := map[string]func(*config.ValidationPolicy){
		"policy argv": func(p *config.ValidationPolicy) { p.Argv = []string{"cargo", "+nightly", "test"} },
		"policy cwd":  func(p *config.ValidationPolicy) { p.CWD = "crate" },
		"probe argv":  func(p *config.ValidationPolicy) { p.IdentityProbes[0].Argv = []string{"rustc", "+nightly", "-vV"} },
		"probe added": func(p *config.ValidationPolicy) {
			p.IdentityProbes = append(p.IdentityProbes, config.IdentityProbe{Name: "x", Argv: []string{"x"}})
		},
		"probe removed":    func(p *config.ValidationPolicy) { p.IdentityProbes = p.IdentityProbes[:1] },
		"argv boundary":    func(p *config.ValidationPolicy) { p.IdentityProbes[0].Argv = []string{"rustc -vV"} },
		"trailing newline": func(p *config.ValidationPolicy) { p.IdentityProbes[0].Argv = []string{"rustc", "-vV\n"} },
	}
	for name, mutate := range same {
		p := clonePolicy(base)
		mutate(&p)
		if got := runcache.PolicyGrantContext(h, p); got != want {
			t.Errorf("%s changed the approved context", name)
		}
	}
	for name, mutate := range differ {
		p := clonePolicy(base)
		mutate(&p)
		if got := runcache.PolicyGrantContext(h, p); got == want {
			t.Errorf("%s kept the approved context", name)
		}
	}
}

func clonePolicy(p config.ValidationPolicy) config.ValidationPolicy {
	p.Argv = append([]string(nil), p.Argv...)
	probes := make([]config.IdentityProbe, len(p.IdentityProbes))
	for i, pr := range p.IdentityProbes {
		probes[i] = config.IdentityProbe{Name: pr.Name, Argv: append([]string(nil), pr.Argv...)}
	}
	p.IdentityProbes = probes
	return p
}
