package checkpointcmd

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
)

// TestDoctorAdvisoryIsNotCheckpointPolicy pins that doctor's size thresholds —
// diagnostic only — never move the checkpoint storage-policy identity.
func TestDoctorAdvisoryIsNotCheckpointPolicy(t *testing.T) {
	h := blake3hash.New()
	base := checkpointPolicyHash(config.Defaults(), h)
	for _, th := range []config.ByteThreshold{config.ThresholdOff(), config.ThresholdAt(1)} {
		cfg := config.Defaults()
		cfg.Doctor.StoreWarningSize = th
		if checkpointPolicyHash(cfg, h) != base {
			t.Errorf("doctor.store_warning_size = %s changed the checkpoint policy hash", th)
		}
		cfg = config.Defaults()
		cfg.Doctor.WatchedRootWarningSize = th
		if checkpointPolicyHash(cfg, h) != base {
			t.Errorf("doctor.watched_root_warning_size = %s changed the checkpoint policy hash", th)
		}
	}
}
