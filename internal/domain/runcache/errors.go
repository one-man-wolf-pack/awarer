package runcache

import "errors"

// ErrNotFound reports that a run id or key pointer does not resolve to a stored
// entry. Callers map it to the CLI not-found exit code.
var ErrNotFound = errors.New("run not found")

// ErrAmbiguousPrefix reports that a short run id prefix matches more than one
// stored run. Callers map it to a usage error the user can fix by being more
// specific.
var ErrAmbiguousPrefix = errors.New("ambiguous run id prefix")

// ErrInvalidPrefix reports that a run id reference is not even a well-formed id
// prefix (empty, too long, or non-hex). It is a user input mistake, not a runtime
// or storage failure, so callers map it to a usage error rather than a generic one.
var ErrInvalidPrefix = errors.New("invalid run id prefix")

// ErrCorruptStore reports that the run store is in an inconsistent state: a key
// pointer to a missing or unreadable entry, a metadata file that will not decode,
// or an output payload whose bytes do not match the recorded hash. It is durable
// corruption, never an ordinary miss, so callers fail loud rather than replaying
// partial or wrong bytes.
var ErrCorruptStore = errors.New("run store is corrupt")

// ErrIncompatibleEntry reports that a recorded run declares a metadata schema this
// build does not speak. The entry is intact and self-describing; awa simply has no
// reader for that shape, so it is never decoded through another type, migrated, or
// guessed.
//
// It is a distinct state, not ErrCorruptStore: lookup treats it as a clean miss so
// the command re-executes, and diagnostics report it apart from damage. It is also
// not disposable — without a reviewed schema nothing can prove what the record
// references, so gc retains and blocks on it rather than reclaiming it, and only an
// explicit user reset removes it. The store owns the version boundary and names it in
// the error text; this doc deliberately does not restate a number that moves.
var ErrIncompatibleEntry = errors.New("run entry uses an incompatible metadata schema")

// ErrObservationUnavailable reports that a recorded run does not expose the
// requested pre/post observation. A post-scan-failed run has no after-observation;
// that is the only case, since every recorded run carries a before-observation. It is
// an honest "this observation was never captured", distinct from ErrCorruptStore (the
// observation should exist but is damaged), so a run-observation reference fails loud
// with the right reason.
var ErrObservationUnavailable = errors.New("run observation is unavailable")
