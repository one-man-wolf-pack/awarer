package cli

import (
	"errors"

	"awarer/internal/app/configcmd"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/probegrants"
	"awarer/internal/output"
)

// Both subcommands act only on the current project's per-user grant; neither runs,
// resolves or edits anything else.
func runConfigProbes(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	if len(args) == 0 {
		return usageErrorf("config probes requires a subcommand: allow or revoke")
	}
	if len(args) > 1 {
		return rejectExtra("config probes "+args[0], args[1])
	}
	switch args[0] {
	case "allow":
		return runConfigProbesAllow(w, opts)
	case "revoke":
		return runConfigProbesRevoke(w, opts)
	default:
		return usageErrorf("unknown config probes subcommand %q: want allow or revoke", args[0])
	}
}

func runConfigProbesAllow(w *output.Writer, opts GlobalOptions) error {
	root, err := currentProjectRoot(opts)
	if err != nil {
		return err
	}
	files, err := configLayerFiles(opts)
	if err != nil {
		return err
	}
	loaded, err := configcmd.LoadLayered(files, configcmd.Overrides{})
	if err != nil {
		return classifyConfigLoad(err)
	}
	hasher := blake3hash.New()
	canon, approved, err := configcmd.AllowProbes(root, loaded.Config(), probegrants.New(hasher), hasher)
	if errors.Is(err, configcmd.ErrNoProbesDeclared) {
		return genericErrorf("config probes allow: %v", err)
	}
	if err != nil {
		// A failure after the record was installed (its directory sync) leaves the new
		// approval on disk, so the message must not claim nothing was recorded.
		return genericErrorf("config probes allow: %v; the approval is not confirmed, check 'awa config effective'", err)
	}
	if opts.JSON {
		return w.JSON("config.probes.allow", struct {
			Root     string                        `json:"root"`
			Policies []configcmd.ProbeApprovalView `json:"policies"`
		}{canon, approved})
	}
	w.Linef("approved identity probes for %s", canon)
	for _, p := range approved {
		w.Linef("  policy %s: argv %s, cwd %q", p.Name, quoteList(p.Argv), p.CWD)
		for _, pr := range p.IdentityProbes {
			w.Linef("    probe %s: %s", pr.Name, quoteList(pr.Argv))
		}
	}
	w.Line("awa run executes these commands before a lookup and after the command, unsandboxed and with your permissions;")
	w.Line("approval covers these exact argv at this path, not what the executables do or later changes to them")
	w.Line("a changed probe or policy needs approval again; withdraw with 'awa config probes revoke'")
	return nil
}

func runConfigProbesRevoke(w *output.Writer, opts GlobalOptions) error {
	root, err := currentProjectRoot(opts)
	if err != nil {
		return err
	}
	canon, removed, err := probegrants.New(blake3hash.New()).Revoke(root)
	if err != nil {
		return genericErrorf("config probes revoke: %v", err)
	}
	if opts.JSON {
		return w.JSON("config.probes.revoke", struct {
			Root    string `json:"root"`
			Removed bool   `json:"removed"`
		}{canon, removed})
	}
	if removed {
		w.Linef("revoked the identity probe grant for %s", canon)
	} else {
		w.Linef("no identity probe grant for %s", canon)
	}
	return nil
}

// A grant always names a project, so a standalone --config file cannot stand in for one.
func currentProjectRoot(opts GlobalOptions) (string, error) {
	proj, err := resolveProject(opts)
	if err != nil {
		return "", err
	}
	layout, err := proj.Paths()
	if err != nil {
		return "", err
	}
	return layout.Root(), nil
}

// renderProbeAuthorization prints the grant as comments after the composed config, so
// it can never be read or pasted as a config setting.
func renderProbeAuthorization(w *output.Writer, v *configcmd.ProbeAuthorizationView) {
	if v == nil {
		return
	}
	w.Line("")
	w.Line("# identity probe authorization: an operator grant held outside the project, not a config layer")
	root := v.Root
	if root == "" {
		root = "<unresolved>"
	}
	w.Linef("# root:  %s", root)
	switch probegrants.State(v.State) {
	case probegrants.StateUnavailable:
		w.Linef("# grant: unavailable (%s): %s", v.Cause, v.Detail())
		w.Line("#        an unreadable grant authorizes nothing; awa run still runs commands, without reuse")
	case probegrants.StateAbsent:
		w.Line("# grant: none")
	default:
		w.Line("# grant: recorded")
	}
	if len(v.Policies) == 0 {
		w.Line("# no validation policy declares identity probes")
		return
	}
	pending := false
	for _, p := range v.Policies {
		state := "approved"
		if !p.Approved {
			state, pending = "not approved", true
		}
		w.Linef("# policy %s: %s", p.Name, state)
	}
	if pending {
		w.Line("# review the probes above, then approve them with 'awa config probes allow'")
	}
}

func inspectProbeAuthorization(opts GlobalOptions, loaded domainconfig.ResolvedConfig) *configcmd.ProbeAuthorizationView {
	root, err := currentProjectRoot(opts)
	if err != nil {
		return nil
	}
	hasher := blake3hash.New()
	v := configcmd.InspectProbes(root, loaded.Config(), probegrants.New(hasher), hasher)
	return &v
}
