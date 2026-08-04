package thinpool

import "testing"

func TestNameFromDevice(t *testing.T) {
	cases := map[string]string{
		"/dev/atlas/atlas-snap-abcd":     "atlas-snap-abcd",
		"/dev/atlas/atlas-datasnap-abcd": "atlas-datasnap-abcd",
		"bare-name":                      "bare-name",
	}
	for input, want := range cases {
		if got := NameFromDevice(input); got != want {
			t.Errorf("NameFromDevice(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsProtected(t *testing.T) {
	for _, name := range []string{"pool0", "atlas-image-ubuntu-24.04", "atlas-image-golden"} {
		if !IsProtected(name) {
			t.Errorf("IsProtected(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"atlas-vm-abcd", "atlas-snap-abcd", "atlas-datasnap-abcd"} {
		if IsProtected(name) {
			t.Errorf("IsProtected(%q) = true, want false", name)
		}
	}
}

// parsePercent is the field that bit real hosts: a blank cell is a fresh pool, not
// a full one, and a malformed cell must not by itself fail a verb.
func TestParsePercent(t *testing.T) {
	cases := map[string]float64{
		" 87.34": 87.34,
		"":       0.0,
		"   ":    0.0,
		"junk":   0.0,
		"0":      0.0,
	}
	for input, want := range cases {
		if got := parsePercent(input); got != want {
			t.Errorf("parsePercent(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestReferenceAndDevicePath(t *testing.T) {
	if got := Reference("atlas-snap-x"); got != "atlas/atlas-snap-x" {
		t.Errorf("Reference = %q", got)
	}
	if got := DevicePath("atlas-snap-x"); got != "/dev/atlas/atlas-snap-x" {
		t.Errorf("DevicePath = %q", got)
	}
	if got := BaseImageLV("golden"); got != "atlas-image-golden" {
		t.Errorf("BaseImageLV = %q", got)
	}
}
