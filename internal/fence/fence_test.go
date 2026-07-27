package fence

import (
	"errors"
	"strings"
	"testing"
)

// The four cases are the whole surface of this package, so they are stated as
// data. The one that would be quietly wrong for a long time is the first: a Boat
// with no store is exactly a Boat that has just lost one.
func TestAllow(t *testing.T) {
	cases := map[string]struct {
		heldEpoch      int64
		held           bool
		requestedEpoch int64
		want           error
	}{
		"no fence held at all": {
			held: false, requestedEpoch: 7, want: ErrNoFence,
		},
		"no fence held, and the request asks for epoch zero": {
			held: false, requestedEpoch: 0, want: ErrNoFence,
		},
		"requested epoch older than held — a start that outlived a migration": {
			heldEpoch: 8, held: true, requestedEpoch: 7, want: ErrStaleEpoch,
		},
		"requested epoch equal to held — the steady state": {
			heldEpoch: 8, held: true, requestedEpoch: 8, want: nil,
		},
		"requested epoch newer than held — a claim this host has not been told yet": {
			heldEpoch: 8, held: true, requestedEpoch: 9, want: nil,
		},
	}
	for name, boot := range cases {
		t.Run(name, func(t *testing.T) {
			err := Allow(boot.heldEpoch, boot.held, boot.requestedEpoch)
			if !errors.Is(err, boot.want) {
				t.Fatalf("Allow(%d, %v, %d) = %v, want %v",
					boot.heldEpoch, boot.held, boot.requestedEpoch, err, boot.want)
			}
		})
	}
}

// The zero epoch is a real value, not a sentinel: whether a fence is held is
// carried by the bool, and nothing may infer "no fence" from the number.
func TestAllowTreatsHeldZeroAsAFenceThatIsHeld(t *testing.T) {
	if err := Allow(0, true, 0); err != nil {
		t.Fatalf("Allow(0, true, 0) = %v, want permission", err)
	}
	if err := Allow(0, true, -1); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("Allow(0, true, -1) = %v, want ErrStaleEpoch", err)
	}
}

// A refusal an operator cannot act on is half a refusal, so the stale error
// carries both numbers.
func TestStaleEpochErrorNamesBothEpochs(t *testing.T) {
	err := Allow(8, true, 7)
	if err == nil {
		t.Fatalf("a stale start was allowed")
	}
	message := err.Error()
	for _, want := range []string{"7", "8"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not name epoch %s", message, want)
		}
	}
}
