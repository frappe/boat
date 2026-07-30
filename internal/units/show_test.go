package units

import (
	"strings"
	"testing"
)

// systemd blocks are separated by a blank line and carry no ordering guarantee
// worth relying on, so the unit each block describes is read from its own Id
// rather than from its position in the answer.
func TestABlockIsIdentifiedByItsOwnIdAndNotItsPosition(t *testing.T) {
	output := "Id=atlas-wake-trap.service\nActiveState=active\nSubState=running\nLoadState=loaded\n"

	liveness := parseLiveness(output)

	if len(liveness) != 1 {
		t.Fatalf("got %+v, want one unit", liveness)
	}
	if liveness[0].Name != "atlas-wake-trap.service" {
		t.Errorf("got name %q, want the Id systemd printed", liveness[0].Name)
	}
}

// `masked`, `error` and `bad-setting` all mean this host HAS something and it is
// not working, which is exactly what supervision is for. Only `not-found` means
// the service is not run here.
func TestOnlyNotFoundMeansThisHostDoesNotRunTheService(t *testing.T) {
	for _, load := range []string{"loaded", "masked", "error", "bad-setting"} {
		output := "Id=atlas-pool.service\nLoadState=" + load + "\nActiveState=inactive\nSubState=dead\n"
		if len(parseLiveness(output)) != 1 {
			t.Errorf("LoadState=%s was dropped, and it means the host has the unit", load)
		}
	}
	output := "Id=atlas-pool.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\n"
	if len(parseLiveness(output)) != 0 {
		t.Error("LoadState=not-found was reported, and it means the host does not run the service")
	}
}

func TestParsingEmptyOutputYieldsAnEmptySetRatherThanNil(t *testing.T) {
	liveness := parseLiveness("")

	if liveness == nil {
		t.Fatal("got nil, which reads as unlooked-at")
	}
	if len(liveness) != 0 {
		t.Errorf("got %+v, want none", liveness)
	}
}

// The template grows one hole per name and the names arrive as parameters, so
// the literal half of the command stays literal — the trust model internal/run
// is built on holds for a variable-length command exactly as for a fixed one.
func TestShowCommandGrowsAHolePerNameAndInterpolatesNone(t *testing.T) {
	names := []string{"atlas-pool.service", "atlas-wake-trap.service"}

	template, parameters := showCommand(names)

	if strings.Count(template, "{}") != len(names) {
		t.Errorf("got %q, want one hole per name", template)
	}
	if len(parameters) != len(names) {
		t.Fatalf("got %d parameters for %d names", len(parameters), len(names))
	}
	for _, name := range names {
		if strings.Contains(template, name) {
			t.Errorf("%q was baked into the template instead of arriving in a hole", name)
		}
	}
}
