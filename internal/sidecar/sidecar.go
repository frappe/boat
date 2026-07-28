// Package sidecar reads the shell KEY=value files the host keeps beside every
// VM — network.env above all.
//
// provision writes that sidecar and the vm-network-up/down hooks read it back,
// which is what lets a host rebuild a VM's networking after a reboot without
// reaching into a database. Boat reads it for the same reason, and for one more:
// it is the WRITER's record of what this UUID owns. Atlas derives the namespace,
// the tap, the veth pair and the per-VM uid from the UUID, and a second
// derivation here would mis-attribute silently the day the two drift — so the
// values are read from the file rather than recomputed.
//
// Everything here is a pure function of the file's text. The read itself needs a
// runner, because the VM tree is 0700 owned by the per-VM uid and an in-process
// open would report "absent" for a file that is plainly there; it therefore
// stays in whichever package already has one.
package sidecar

import "strings"

// Parse reads the file the way sourcing it would: blank lines and comments are
// skipped and surrounding quotes stripped. provision writes bare values, but a
// reader that only handles its own writer's output is a reader that fails on the
// first hand-edited host.
func Parse(text string) map[string]string {
	values := map[string]string{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, found := strings.Cut(line, "="); found {
			values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
		}
	}
	return values
}

// Value is one key out of the file, empty when it is not there. Empty is
// deliberately not distinguished from absent: every caller treats a key the
// writer never wrote and a key it wrote blank the same way.
func Value(text string, key string) string {
	return Parse(text)[key]
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}
