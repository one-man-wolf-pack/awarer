//go:build !darwin && !linux && !freebsd

package lockfile

// ProcessAlive cannot determine liveness on platforms without the Unix null-signal
// probe, so it conservatively reports true: a lock whose owner liveness is unknown
// is treated as possibly active and never removed on age-from-this-host grounds.
// Cross-host age-threshold staleness in Classify still applies.
func ProcessAlive(pid int) bool {
	return true
}
