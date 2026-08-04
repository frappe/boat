package snapshot

import "testing"

// parseCPUSignature must stop at the blank line ending the first processor block —
// on a many-thread host the rest is identical text not worth walking — and hash
// the flag set deterministically.
func TestParseCPUSignature(t *testing.T) {
	signature := parseCPUSignature(cpuinfoFixture)
	if signature.CPUModel != "Intel(R) Xeon(R) CPU" {
		t.Errorf("cpu model = %q", signature.CPUModel)
	}
	if signature.Microcode != "0xf0" {
		t.Errorf("microcode = %q", signature.Microcode)
	}
	if signature.CPUFlagsSHA256 != "4016e4ce6f37fb56" {
		t.Errorf("cpu flags digest = %q", signature.CPUFlagsSHA256)
	}
}

// The flag digest is order-independent (the set is sorted before hashing) but
// content-sensitive, so two hosts with the same CPUID surface match and any
// difference does not.
func TestDigestFlagsIsSortedAndStable(t *testing.T) {
	if digestFlags("fpu vme de") != digestFlags("de vme fpu") {
		t.Error("flag digest depends on order")
	}
	if digestFlags("fpu vme de") == digestFlags("fpu vme") {
		t.Error("flag digest ignored a missing flag")
	}
}

func TestParseFirecrackerVersion(t *testing.T) {
	cases := map[string]string{
		"Firecracker v1.16.0\n": "v1.16.0",
		"Firecracker v1.13.1":   "v1.13.1",
		"weird output":          "weird output",
		"":                      "",
	}
	for input, want := range cases {
		if got := parseFirecrackerVersion(input); got != want {
			t.Errorf("parseFirecrackerVersion(%q) = %q, want %q", input, got, want)
		}
	}
}
