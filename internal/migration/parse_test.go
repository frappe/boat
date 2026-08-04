package migration

import "testing"

// These lock the pure parsers — the bits that break on a kernel dm-clone or lvs
// format change — with no host, the discipline lvm.py keeps for the same fields.

func TestParsePercent(t *testing.T) {
	for _, testCase := range []struct {
		cell string
		want float64
	}{
		{"", 0.0},         // a fresh pool prints blank → the ${pct:-0} default
		{"  ", 0.0},       // whitespace only
		{"87.34", 87.34},  // an ordinary fill
		{" 42.00 ", 42.0}, // padded, as lvs prints it
		{"garbage", 0.0},  // unreadable → 0, not a crash
		{"100", 100.0},    // integer form
	} {
		if got := parsePercent(testCase.cell); got != testCase.want {
			t.Errorf("parsePercent(%q) = %v, want %v", testCase.cell, got, testCase.want)
		}
	}
}

func TestHydrationPercent(t *testing.T) {
	for _, testCase := range []struct {
		status string
		want   int
	}{
		{"0 20971520 clone 8/1024 32768 320/640 0 rw", 50}, // 2nd pair is hydrated/total
		{"0 20971520 clone 8/1024 32768 640/640 0 rw", 100},
		{"0 20971520 clone 8/1024 32768 0/640 0 rw", 0},
		{"0 20971520 clone 8/1024 32768 640/0 0 rw", 100}, // total 0 → done, no divide-by-zero
	} {
		got, err := hydrationPercent(testCase.status)
		if err != nil {
			t.Fatalf("hydrationPercent(%q): %v", testCase.status, err)
		}
		if got != testCase.want {
			t.Errorf("hydrationPercent(%q) = %d, want %d", testCase.status, got, testCase.want)
		}
	}
	// A line with fewer than two a/b pairs is not a clone status.
	if _, err := hydrationPercent("0 20971520 linear /dev/atlas/x 0"); err == nil {
		t.Error("hydrationPercent parsed a non-clone status")
	}
}

func TestFullyHydrated(t *testing.T) {
	if !fullyHydrated("0 20971520 clone 8/1024 32768 640/640 0 rw") {
		t.Error("640/640 should be fully hydrated")
	}
	if fullyHydrated("0 20971520 clone 8/1024 32768 639/640 0 rw") {
		t.Error("639/640 is not fully hydrated")
	}
	// An unparseable status is NOT fully hydrated — refusing a collapse on it is safe.
	if fullyHydrated("garbage") {
		t.Error("an unparseable status must not read as fully hydrated")
	}
}

func TestCloneTableFields(t *testing.T) {
	table := "0 20971520 clone /dev/atlas/meta /dev/atlas/dest 43:0 32768"
	if got := cloneTableDest(table); got != "/dev/atlas/dest" {
		t.Errorf("cloneTableDest = %q, want the field-4 dest", got)
	}
	if got := cloneTableSource(table); got != "43:0" {
		t.Errorf("cloneTableSource = %q, want the field-5 source", got)
	}
	sectors, err := cloneSectors(table)
	if err != nil || sectors != 20971520 {
		t.Errorf("cloneSectors = %d, %v; want 20971520", sectors, err)
	}
	if !isLinearTable("0 20971520 linear /dev/atlas/dest 0") {
		t.Error("a linear table should be recognised")
	}
	if isLinearTable(table) {
		t.Error("a clone table is not linear")
	}
}

func TestListeningOn(t *testing.T) {
	output := "State  Recv-Q Send-Q Local Address:Port\nLISTEN 0 10 0.0.0.0:11165 0.0.0.0:*\nLISTEN 0 10 [::]:22 [::]:*"
	if !listeningOn(output, 11165) {
		t.Error("11165 is listening")
	}
	if listeningOn(output, 22) != true {
		t.Error("22 is listening (IPv6 form)")
	}
	if listeningOn(output, 1116) {
		t.Error("1116 must not match as a prefix of :11165")
	}
	if listeningOn(output, 9999) {
		t.Error("9999 is not listening")
	}
}
