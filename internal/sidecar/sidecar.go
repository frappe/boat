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

// Upsert returns text with key=value set — replacing the existing line in place,
// keeping the order of everything around it, or appending the line when the key
// is absent. The output always ends in one newline.
//
// This is how a reserved IP's durable flag lands in a running VM's network.env
// at attach time: the sidecar is the host's own record of what a VM owns, so a
// later cold boot re-creates the 1:1 NAT from disk exactly as provision would.
// The caller does the atomic write; this is only the text. Ported from
// upsert_network_env in scripts/lib/atlas/network_env.py.
func Upsert(text string, key string, value string) string {
	lines := splitLines(text)
	rendered := key + "=" + value
	for index, line := range lines {
		if lineKey(line) == key {
			lines[index] = rendered
			return strings.Join(lines, "\n") + "\n"
		}
	}
	return strings.Join(append(lines, rendered), "\n") + "\n"
}

// Remove returns text with any key= line taken out — the detach twin of Upsert,
// so a detached VM's env no longer carries the key and a reboot brings it up
// without it. An empty result is the empty string, not a lone newline, so a file
// emptied of its last key reads as absent rather than blank. Ported from
// remove_network_env.
func Remove(text string, key string) string {
	lines := splitLines(text)
	kept := lines[:0]
	for _, line := range lines {
		if lineKey(line) != key {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// lineKey is the KEY of a KEY=value line, or "" for a blank line, a comment, or
// anything else that names no key. A value carrying an `=` keeps its own value
// (only the first `=` splits the key off), and surrounding whitespace on the key
// is trimmed — the same read Parse does, so Upsert replaces exactly the line
// Value would have read.
func lineKey(line string) string {
	stripped := strings.TrimSpace(line)
	if stripped == "" || strings.HasPrefix(stripped, "#") {
		return ""
	}
	key, _, _ := strings.Cut(stripped, "=")
	return strings.TrimSpace(key)
}

// splitLines splits text into lines the way Python's str.splitlines does for the
// newline-terminated files Boat handles: the terminating newline of the last
// line produces no trailing empty element, an empty string is no lines at all,
// and a `\r\n` writer's carriage return is dropped. Matching that exactly is
// what lets Upsert/Remove reproduce the Python transform byte for byte.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSuffix(line, "\r")
	}
	if last := len(lines) - 1; lines[last] == "" {
		lines = lines[:last]
	}
	return lines
}
