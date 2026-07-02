package daemon

import (
	"testing"

	"nft-forward/internal/forward"
)

// A failed counters send must not lose the deltas: reAddCounters rewinds the
// cursor so the next sample re-reports them, and a successful send (no rollback)
// leaves the cursor advanced so unchanged counters yield nothing.
func TestCounterSamplesRollbackOnFailedSend(t *testing.T) {
	fake := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = fake

	fake.counters = []forward.Counter{{Proto: "tcp", ListenPort: 12000, BytesUp: 100, BytesDown: 50}}

	samples := d.counterSamples()
	if len(samples) != 1 || samples[0].BytesUp != 100 || samples[0].BytesDown != 50 {
		t.Fatalf("first sample = %+v, want up=100 down=50", samples)
	}

	// Simulate a failed send: roll the deltas back.
	d.reAddCounters(samples)

	// Kernel counters unchanged → the rolled-back delta must be re-reported.
	samples2 := d.counterSamples()
	if len(samples2) != 1 || samples2[0].BytesUp != 100 || samples2[0].BytesDown != 50 {
		t.Fatalf("after rollback re-report = %+v, want up=100 down=50", samples2)
	}

	// This send "succeeds" (no rollback); with no new bytes the next poll is empty.
	if got := d.counterSamples(); len(got) != 0 {
		t.Fatalf("after commit with no new bytes, want no samples, got %+v", got)
	}
}
