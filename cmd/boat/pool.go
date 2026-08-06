package main

import (
	"context"
	"flag"
	"io"

	"github.com/frappe/boat/internal/bootstrap"
	"github.com/frappe/boat/internal/run"
)

// poolCommand re-asserts this host's LVM thin pool — `boat pool`, the standalone
// verb atlas-pool.service runs on every boot in place of the Python one-liner it
// carried, `python -c "from atlas.lvm import ThinPool; ThinPool().ensure()"`.
//
// It is a boot-time oneshot, not a client of the daemon: the pool is what every
// VM disk is carved from, so it must be re-asserted before the daemon and before
// any firecracker-vm@ unit that orders `After=` it. On a stock cloud droplet a
// reboot survives the backing file but drops its loop binding, so the pool's VG
// is gone until this re-binds it — which is the whole reason the unit exists and
// runs on each boot rather than once at bootstrap.
//
// Idempotent, and carrying `boat bootstrap`'s backing-file limitation: only the
// loopback backing is re-asserted (see bootstrap.EnsureThinPool). It takes no
// flags; `boat pool --help` exits 0, so an operator can prove the verb is
// installed the same way reset.py proves the others are.
func poolCommand(arguments []string, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("boat pool", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	if err := flags.Parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	if err := bootstrap.EnsureThinPool(context.Background(), run.NewRunner(errorOutput)); err != nil {
		return reportError(errorOutput, err)
	}
	_, _ = io.WriteString(errorOutput, "thin pool re-asserted\n")
	return exitSuccess
}
