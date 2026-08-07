package cli

import (
	"strings"

	"awarer/internal/app/configcmd"
	doctorapp "awarer/internal/app/doctor"
	domdoctor "awarer/internal/domain/doctor"
	"awarer/internal/infra/lockfile"
	"awarer/internal/output"
)

// runDoctor handles "awa doctor". It inspects the project's durable .awa state and
// prints a concise report; with --repair it also performs the safe, mechanical
// repairs. Verification depth is raised by the global --strict (trust-strict) flag,
// which for doctor means recomputing hashes rather than trusting stat signatures.
// The command takes only the local --repair flag; --root/--config/--json/--strict
// are global. It does not consume operands.
func runDoctor(w *output.Writer, inv invocation) error {
	repair := false
	args := inv.args
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, _, hasValue := splitFlag(tok)
		switch name {
		case "--repair":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			repair = true
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for doctor", tok)
			}
			return usageErrorf("unexpected argument %q for doctor", tok)
		}
	}

	proj, err := resolveProject(inv.options)
	if err != nil {
		return err
	}
	layout, err := proj.Paths()
	if err != nil {
		return err
	}

	// The project supplies the storage location; the shared awa.toml and local
	// .awa/config.toml layer under any --config override, so doctor validates the
	// same composed config the user points it at. An invalid layer is a config error
	// (exit 4), surfaced before any check runs.
	files, err := projectLayerFiles(layout, inv.options)
	if err != nil {
		return err
	}
	loaded, err := configcmd.LoadLayered(files, trustModeOverride(inv.options))
	if err != nil {
		return classifyConfigLoad(err)
	}
	cfg := loaded.Config()

	// Doctor's "strict" is the global trust-strict mode: do not trust stat-signature
	// reuse for integrity claims; recompute hashes where the scanner/index supports
	// it.
	strict := inv.options.has(capTrustMode) && inv.options.TrustMode == TrustStrict

	svc := doctorapp.New(doctorapp.Deps{
		Locks: newCollectorLocker(inv.invCtx(), layout, lockfile.OpDoctorRepair, cfg.Locks.Timeout.Duration()),
	})
	res, err := svc.Run(inv.invCtx(), doctorapp.Request{
		Project:  proj,
		Resolved: loaded,
		Strict:   strict,
		Repair:   repair,
	})
	if err != nil {
		// A failure that prevented diagnosis or the repair pass at all (e.g. the layout
		// could not be resolved, or --repair could not acquire its lock before a
		// collector's timeout). Ordinary corruption is reported as findings, not here.
		if c := interruptError(err); c != nil {
			return c
		}
		if coded := lockErrorCode(err); coded != nil {
			return coded
		}
		return genericErrorf("%v", err)
	}

	if inv.options.JSON {
		if err := w.JSON("doctor", doctorViewOf(res)); err != nil {
			return err
		}
	} else {
		renderDoctor(w, res)
	}

	// Exit code is the diagnosis, not an awa failure: like "awa run", doctor's own
	// outcome drives the process exit with no "awa: " error line. Unrepaired errors
	// or unrepaired repairable findings mean local state needs an action (exit 5) —
	// which is not the same as damage, since an unrepaired guard file earns it too; a
	// healthy or warnings-only run exits 0.
	if res.NeedsAttention() {
		return &commandExit{code: int(ExitStateActionRequired)}
	}
	return nil
}

// doctorView is the stable JSON shape of a doctor report. Findings carry their own
// severity, so a single list serves machine consumers that filter by severity or
// repairability; the summary mirrors the derived counts.
type doctorView struct {
	Health   string              `json:"health"`
	Summary  doctorSummaryView   `json:"summary"`
	Findings []doctorFindingView `json:"findings"`
}

type doctorSummaryView struct {
	Passed     int `json:"passed"`
	Warnings   int `json:"warnings"`
	Errors     int `json:"errors"`
	Repairable int `json:"repairable"`
	Repaired   int `json:"repaired"`
}

type doctorFindingView struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Subsystem  string `json:"subsystem"`
	Path       string `json:"path,omitempty"`
	Subject    string `json:"subject"`
	Message    string `json:"message"`
	Repairable bool   `json:"repairable"`
	Repaired   bool   `json:"repaired"`
}

func doctorViewOf(res domdoctor.DoctorResult) doctorView {
	s := res.Summary()
	v := doctorView{
		Health: res.Health().String(),
		Summary: doctorSummaryView{
			Passed:     s.Passed,
			Warnings:   s.Warnings,
			Errors:     s.Errors,
			Repairable: s.Repairable,
			Repaired:   s.Repaired,
		},
		Findings: []doctorFindingView{},
	}
	for _, f := range res.Findings() {
		v.Findings = append(v.Findings, doctorFindingView{
			Code:       f.Code().String(),
			Severity:   f.Severity().String(),
			Subsystem:  string(f.Subsystem()),
			Path:       f.Path(),
			Subject:    f.Subject(),
			Message:    f.Message(),
			Repairable: f.Repairable(),
			Repaired:   f.Repaired(),
		})
	}
	return v
}

// renderDoctor prints the human report: a one-line verdict for a healthy project,
// or the verdict plus one line per finding grouped by the order they were found.
// The tag favors the action a reader should take — [repaired] for a fixed finding,
// [repairable] for one that --repair would fix, otherwise the severity.
func renderDoctor(w *output.Writer, res domdoctor.DoctorResult) {
	s := res.Summary()
	w.Linef("health: %s", res.Health().String())

	findings := res.Findings()
	if len(findings) > 0 {
		w.Line("")
		for _, f := range findings {
			w.Linef("[%s] %s %s — %s", findingTag(f), f.Code(), f.Subject(), f.Message())
		}
		w.Line("")
	}
	w.Linef("checks: %d passed, %d warnings, %d errors", s.Passed, s.Warnings, s.Errors)
	// Summary.Repairable counts every repairable finding including those already
	// fixed, so the still-fixable tally subtracts the repaired ones.
	if s.Repairable > 0 {
		w.Linef("repairs: %d repaired, %d repairable", s.Repaired, s.Repairable-s.Repaired)
	}
}

// findingTag chooses the bracketed label for a finding line.
func findingTag(f domdoctor.Finding) string {
	switch {
	case f.Repaired():
		return "repaired"
	case f.Repairable():
		return "repairable"
	default:
		return f.Severity().String()
	}
}
