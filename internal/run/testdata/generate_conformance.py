"""Emit a Go conformance table straight out of CPython's shlex.

Every expectation in the generated file is what this interpreter's
shlex.quote / shlex.split actually returned, so the Go port is tested against
the real oracle rather than against someone's memory of it.
"""

import json
import shlex
import sys

QUOTE_VALUES = [
	# --- the shapes this codebase actually passes ---
	"",
	"simple-path",
	"/var/lib/atlas/virtual-machines/6d0a1e2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b",
	"6d0a1e2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b",
	"firecracker-vm@6d0a1e2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b.service",
	"/var/lib/atlas/virtual-machines/6d0a/jail/firecracker/6d0a/root/run",
	"firecracker.socket",
	"http://localhost/vm",
	"PATCH",
	"0644",
	"0700",
	"443",
	"tap0",
	"2400:dead::1/128",
	"1G",
	"%+,-./0123456789:=@ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz",
	# --- nft fragments: braces and semicolons must survive ---
	"{ type filter hook forward priority filter; policy accept; }",
	"add rule inet atlas forward ip6 daddr { 2400:dead::1, 2400:dead::2 } accept",
	"{ nft brace }",
	"{}",
	# --- Firecracker API bodies ---
	'{"state":"Paused"}',
	'{"snapshot_type": "Full", "snapshot_path": "snapshot/vmstate.bin", "mem_file_path": "snapshot/mem.bin"}',
	'{"latest": {"meta-data": {"hostname": "vm-1", "public-keys": ["ssh-ed25519 AAAA me@host"]}}}',
	# --- file content that goes through install ---
	"[Service]\nTimeoutStopSec=30\n",
	"TAP_DEVICE=tap0\nNETNS=atlas-6d0a\n",
	# --- whitespace ---
	"a b",
	" leading",
	"trailing ",
	" ",
	"a\tb",
	"a\nb",
	"a\rb",
	"\n",
	"--flag=value with spaces",
	# --- quotes and escapes ---
	"it's",
	"'",
	"''",
	'"',
	'a"b',
	"a\\b",
	"back\\slash'and\"quote",
	# --- the injection battery ---
	"a; rm -rf /",
	"a | tee /etc/passwd",
	"$(whoami)",
	"`id`",
	"a && reboot",
	"' ; echo pwned ; '",
	"../../etc/shadow",
	"$HOME",
	"~/atlas",
	"*",
	"?",
	"#comment",
	"!bang",
	"a>b",
	"a<b",
	"e2fsprogs>=1.46",
	# --- non-ASCII ---
	"ünïcodé",
	"日本語",
]

SPLIT_LINES = [
	"sudo systemctl stop nginx",
	"ip link set tap0 up",
	"echo 'a b'",
	"echo ''",
	'echo "a b"',
	'echo "a\\"b"',
	'echo "a\\b"',
	'echo "a\\\\b"',
	"echo a\\ b",
	"echo '$(id)'",
	"nft add chain inet atlas forward '{ type filter hook forward priority filter; policy accept; }'",
	"sudo install -m 0644 /tmp/boat-install-123 /etc/atlas/network.env",
	"  spaced   out   words  ",
	"a\tb\nc",
	"carriage\rreturn separates",
	"\rleading carriage return",
	"vertical\vtab and\fform feed do not separate",
	"echo a#b # not-a-comment",
	"%\\/\r/",
	"''",
	"a''",
	"'a'b'c'",
	'"" x',
	'a"b c"d',
	"x=1 y=2",
	"printf '%s\\n' hello",
	"",
	"   ",
	"sudo sh -c " + shlex.quote("cd /x && curl --fail --unix-socket firecracker.socket -d " + shlex.quote('{"state":"Paused"}')),
	"tail -c +1024 '/var/lib/atlas/images/a b/packed' | zstd -dc -f > /tmp/vmlinux",
]

SPLIT_ERRORS = ['echo "unterminated', "echo 'unterminated", "echo trailing\\", 'echo "a\\']


def go(value):
	return json.dumps(value, ensure_ascii=True)


def main():
	out = []
	out.append("package run")
	out.append("")
	out.append("// Code generated from CPython " + ".".join(map(str, sys.version_info[:3])) + " shlex. DO NOT EDIT.")
	out.append("//")
	out.append("// Every expectation below is what shlex.quote / shlex.split actually returned")
	out.append("// for that input, so this file is the conformance oracle for Quote and Split.")
	out.append("// Regenerate with:  python3 testdata/generate_conformance.py > shlex_conformance_test.go")
	out.append("")
	out.append("var quoteConformance = []struct{ value, quoted string }{")
	for value in QUOTE_VALUES:
		out.append("\t{%s, %s}," % (go(value), go(shlex.quote(value))))
	out.append("}")
	out.append("")
	out.append("var splitConformance = []struct {")
	out.append("\tline string")
	out.append("\targv []string")
	out.append("}{")
	for line in SPLIT_LINES:
		argv = shlex.split(line)
		rendered = ", ".join(go(token) for token in argv)
		out.append("\t{%s, []string{%s}}," % (go(line), rendered))
	out.append("}")
	out.append("")
	out.append("// splitConformanceErrors are lines CPython's shlex refuses outright.")
	out.append("var splitConformanceErrors = []string{")
	for line in SPLIT_ERRORS:
		try:
			shlex.split(line)
		except ValueError:
			out.append("\t%s," % go(line))
		else:
			raise SystemExit("expected %r to fail" % line)
	out.append("}")
	out.append("")
	sys.stdout.write("\n".join(out))

	# Round-trip check on the oracle itself: whatever quote emits, split returns.
	for value in QUOTE_VALUES:
		assert shlex.split(shlex.quote(value)) == [value], value


main()
