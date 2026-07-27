// Package version carries the identity of this build.
//
// Every systemd unit on a host runs this same binary, so one string describes
// the whole host's Boat surface — which is what lets Atlas treat version drift
// as ordinary observed state rather than a separate bookkeeping channel.
package version

// Version is stamped at link time by the Makefile
// (-ldflags "-X github.com/frappe/boat/internal/version.Version=..."). An
// unstamped build says so plainly rather than claiming a release number.
var Version = "dev"
