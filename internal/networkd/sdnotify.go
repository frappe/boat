// Minimal sd_notify (systemd's ready/watchdog/stopping protocol), ported from
// sdnotify.py. Stdlib only — Taste.md's "don't import, copy": ~30 lines of
// AF_UNIX datagram code is not worth a systemd binding dependency, and boat's
// go.mod stays as small as the memberlist cutover already forced it to grow.
//
// systemd sets NOTIFY_SOCKET in the unit's environment; the daemon sends a single
// AF_UNIX datagram with newline-separated KEY=VALUE fields. READY=1 announces the
// service is up (Type=notify holds the unit "activating" until it arrives);
// WATCHDOG=1 pats the WatchdogSec timer; STOPPING=1 announces an orderly shutdown
// so a SIGTERM exit reads as "inactive", not "failed".
//
// Every helper is a no-op when NOTIFY_SOCKET is unset, so `boat networkd` runs
// unchanged outside systemd (a developer running it by hand). A real socket error
// is returned loud so a misconfigured unit fails rather than silently going dark.
package networkd

import (
	"net"
	"os"
)

const (
	notifyReady    = "READY=1"
	notifyWatchdog = "WATCHDOG=1"
	notifyStopping = "STOPPING=1"
)

// sdNotify sends a single sd_notify message to $NOTIFY_SOCKET. It returns (false,
// nil) when the variable is unset — running outside systemd, a silent no-op — and
// (true, nil) on a sent datagram. A transport error is surfaced.
func sdNotify(message string) (bool, error) {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return false, nil
	}
	// Abstract socket (Linux): systemd hands the name with a leading '@' which
	// stands in for the NUL byte an abstract AF_UNIX address begins with;
	// otherwise it is a filesystem path under /run/systemd/.
	if address[0] == '@' {
		address = "\x00" + address[1:]
	}
	connection, err := net.Dial("unixgram", address)
	if err != nil {
		return false, err
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(message)); err != nil {
		return false, err
	}
	return true, nil
}

// NotifyReady announces the daemon is up. systemd transitions the unit to active
// on receipt.
func NotifyReady() (bool, error) { return sdNotify(notifyReady) }

// NotifyWatchdog pats the watchdog. The loop calls it once per tick, well under
// the unit's WatchdogSec, so a wedged loop is relaunched.
func NotifyWatchdog() (bool, error) { return sdNotify(notifyWatchdog) }

// NotifyStopping announces an orderly shutdown, so systemd marks the unit inactive
// rather than failed when the process exits after a SIGTERM.
func NotifyStopping() (bool, error) { return sdNotify(notifyStopping) }
