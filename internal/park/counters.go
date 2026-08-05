package park

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// Counters reads every wake counter on this host in one call, as a map from VM
// UUID to the number of packets that counter has trapped.
//
// One command for the whole host, not one per VM: this runs about once a second
// forever, and the cost of the reflex has to be independent of how many VMs are
// asleep. `nft list counters` reports named counters only, which is exactly why
// the trap's counter is a named one.
//
// Untraced by the caller's choice of runner rather than by anything here: a poll
// that wrote a line a second would put tens of thousands of lines a day into the
// journal and bury the one line that matters. NewTrap makes that structural.
func Counters(ctx context.Context, runner *run.Runner) (map[string]int64, error) {
	return newParker(runner).counters(ctx)
}

func (parker *parker) counters(ctx context.Context) (map[string]int64, error) {
	// CHECKED, unlike the read this replaced.
	//
	// `nft … table inet atlas` exits non-zero both for a host that has no such
	// table and for a read this daemon was not allowed to make, and it complains on
	// stderr either way — so there is no rule that tells the two apart, which is
	// why run.Probe names this exact command as one that must not be asked as a
	// probe. RunUnchecked answered both with ("", nil), and a caller that dutifully
	// checked the error then read an empty counter listing off a host full of
	// sleeping VMs: no wake, ever, and not one line in the journal to say why.
	//
	// The table is host floor rather than something that comes and goes —
	// bootstrap creates it, the boot sweep re-creates it before the first tick, and
	// every park asserts it — so a non-zero exit here is a fault, and a fault worth
	// a line per tick until somebody fixes it. tick logs it and polls again a
	// second later, which is the same tolerance parseCounters already has for
	// output it cannot read.
	output, err := parker.commands.Run(ctx, "sudo nft -j list counters table inet atlas")
	if err != nil {
		return nil, err
	}
	return parseCounters(output)
}

// parseCounters reads `nft -j list counters`.
//
// The JSON is walked one element at a time rather than decoded into a fixed
// shape, because the array mixes metainfo with counters and nft is free to add
// element kinds this does not know. An element that will not decode is skipped,
// so a future nft cannot stop a host waking its VMs.
//
// Malformed output IS an error, unlike the Python original which swallowed it.
// A tick logs it and polls again a second later, so nothing is lost by saying
// so, and a genuinely broken nft would otherwise look exactly like a host with
// no sleeping VMs.
func parseCounters(output string) (map[string]int64, error) {
	counters := map[string]int64{}
	if strings.TrimSpace(output) == "" {
		return counters, nil
	}
	var listing struct {
		Objects []json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		return counters, fmt.Errorf("read the wake counters: %w", err)
	}
	for _, object := range listing.Objects {
		var element struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets int64  `json:"packets"`
			} `json:"counter"`
		}
		if json.Unmarshal(object, &element) != nil || element.Counter == nil {
			continue
		}
		if uuid, ours := UUIDForCounter(element.Counter.Name); ours {
			counters[uuid] = element.Counter.Packets
		}
	}
	return counters, nil
}
