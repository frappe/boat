package fcattach

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// These tests assert the command sequence Find emits and the answer it draws
// from each reply, because on a machine with no Firecracker that sequence is the
// whole of the behaviour. The canned replies are real: a real ss line, real curl
// exit codes.

const testUUID = "11111111-2222-3333-4444-555555555555"

// testFiles spells the layout out rather than deriving it, so the golden command
// lines below read like the lines an operator sees in a Task log.
func testFiles(uuid string) socketFiles {
	jailRoot := "/var/lib/atlas/virtual-machines/" + uuid + "/jail/firecracker/" + uuid + "/root"
	return socketFiles{
		socket:    jailRoot + "/run/firecracker.socket",
		directory: jailRoot + "/run",
		name:      "firecracker.socket",
	}
}

// probeCommand is the rendered liveness probe: the cd-and-relative-name dance,
// wrapped in the one `sudo sh -c` that makes the cd and the curl share a shell.
func probeCommand(files socketFiles) string {
	return fmt.Sprintf(
		"sudo sh -c cd %s && curl --silent --show-error --max-time 2 --unix-socket %s %s",
		files.directory, files.name, instanceInfoURL,
	)
}

func existsCommand(files socketFiles) string { return "? sudo test -S " + files.socket }

func processCommand(files socketFiles) string {
	return "sudo ss --no-header --unix --listening --processes src " + files.socket
}

// ssLine is real `ss --no-header --unix --listening --processes` output for a
// jailed Firecracker, column padding and all.
func ssLine(files socketFiles) string {
	return "u_str LISTEN 0      128           " + files.socket +
		" 3418239              * 0    users:((\"firecracker\",pid=15843,fd=8))\n"
}

// fakeCommands records every rendered command and answers it from a script. A
// recorded line carries "? " when the command was a boolean gate, so the trace
// shows which parts of the sequence were probes and which were commands whose
// failure means something.
type fakeCommands struct {
	trace   []string
	gates   map[string]bool
	outputs map[string]string
	errors  map[string]error
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{gates: map[string]bool{}, outputs: map[string]string{}, errors: map[string]error{}}
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, command)
	return fake.outputs[command], fake.errors[command]
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "? "+command)
	return fake.gates[command]
}

// render substitutes each {} with its parameter the way run.Substitute does,
// minus the shell quoting — every value here is a path or a URL, and an unquoted
// line is the one a reader can compare to the sudoers entry by eye. It panics on
// an arity mismatch, which catches a miscounted template for free.
func render(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(parameters) {
		panic(fmt.Sprintf("%q: %d placeholders, %d parameters", template, len(parts)-1, len(parameters)))
	}
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			fmt.Fprintf(&builder, "%v", parameters[index])
		}
	}
	return builder.String()
}

func assertTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.trace, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

func findWith(fake *fakeCommands) (Process, bool, error) {
	return find(context.Background(), fake, testFiles(testUUID), testUUID)
}

func TestFindAttachesToAFirecrackerThatAnswersOnItsSocket(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.gates["sudo test -S "+files.socket] = true
	fake.outputs[probeCommand(files)] = `{"app_name":"Firecracker","id":"` + testUUID + `","state":"Running"}`
	fake.outputs[processCommand(files)] = ssLine(files)

	process, found, err := findWith(fake)

	if err != nil || !found {
		t.Fatalf("Find = (%+v, %v, %v), want a live process", process, found, err)
	}
	if process.UUID != testUUID || process.APISocket != files.socket || process.Pid != 15843 {
		t.Errorf("Find = %+v, want uuid %s pid 15843 socket %s", process, testUUID, files.socket)
	}
	assertTrace(t, fake, existsCommand(files), probeCommand(files), processCommand(files))
}

// An absent socket is the ordinary answer for every stopped and sleeping VM on
// the host, so it costs one `test` and never reaches curl.
func TestFindReportsNoProcessWhenTheSocketIsAbsent(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.gates["sudo test -S "+files.socket] = false

	process, found, err := findWith(fake)

	if found || err != nil {
		t.Fatalf("Find = (%+v, %v, %v), want (zero, false, nil)", process, found, err)
	}
	assertTrace(t, fake, existsCommand(files))
}

// The case the whole package exists for: a unix socket inode outlives the
// process that bound it, so a Firecracker that died leaves a socket that stat is
// happy with and nothing is listening on. Existence is not liveness.
func TestFindReportsNoProcessWhenTheSocketExistsButRefuses(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.gates["sudo test -S "+files.socket] = true
	fake.errors[probeCommand(files)] = &run.CommandError{
		Argv:     []string{"sudo", "sh", "-c", "cd … && curl …"},
		ExitCode: curlCouldNotConnect,
		Output:   "curl: (7) Failed to connect to localhost port 80 after 0 ms: Couldn't connect to server\n",
	}

	process, found, err := findWith(fake)

	if found || err != nil {
		t.Fatalf("Find = (%+v, %v, %v), want (zero, false, nil)", process, found, err)
	}
	assertTrace(t, fake, existsCommand(files), probeCommand(files))
}

// Every other non-zero exit is a failure to observe, and Boat says so instead of
// reporting the VM as not running — a false "nothing is here" is how a
// controller decides a live VM is down.
func TestFindReportsAnErrorWhenTheProbeItselfFails(t *testing.T) {
	for name, probeError := range map[string]error{
		"sudo denied":  &run.CommandError{ExitCode: 1, Output: "sudo: a password is required\n"},
		"curl missing": &run.CommandError{ExitCode: 127, Output: "sh: 1: curl: not found\n"},
		"timed out":    &run.CommandError{ExitCode: 28, Output: "curl: (28) Operation timed out\n"},
	} {
		t.Run(name, func(t *testing.T) {
			files := testFiles(testUUID)
			fake := newFakeCommands()
			fake.gates["sudo test -S "+files.socket] = true
			fake.errors[probeCommand(files)] = probeError

			process, found, err := findWith(fake)

			if err == nil {
				t.Fatalf("Find = (%+v, %v, nil), want the probe failure reported", process, found)
			}
			if found {
				t.Error("Find reported a process it never reached")
			}
			if !strings.Contains(err.Error(), files.socket) {
				t.Errorf("error %q does not name the socket it failed on", err)
			}
		})
	}
}

// The pid is a diagnostic, not the liveness claim: the socket already answered,
// so a host where ss is unavailable still yields an attachable process.
func TestFindStillAttachesWhenThePidCannotBeDetermined(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.gates["sudo test -S "+files.socket] = true
	fake.errors[processCommand(files)] = &run.CommandError{ExitCode: 127, Output: "sudo: ss: command not found\n"}

	process, found, err := findWith(fake)

	if err != nil || !found {
		t.Fatalf("Find = (%+v, %v, %v), want a live process", process, found, err)
	}
	if process.Pid != 0 {
		t.Errorf("Pid = %d, want 0 when it could not be determined", process.Pid)
	}
}

func TestParsePidReadsSsAndIgnoresAnythingElse(t *testing.T) {
	files := testFiles(testUUID)
	for name, testCase := range map[string]struct {
		output string
		want   int
	}{
		"real ss line":      {ssLine(files), 15843},
		"header only":       {"Netid State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n", 0},
		"no process column": {"u_str LISTEN 0 128 " + files.socket + " 3418239 * 0\n", 0},
		"empty":             {"", 0},
	} {
		t.Run(name, func(t *testing.T) {
			if pid := parsePid(testCase.output); pid != testCase.want {
				t.Errorf("parsePid = %d, want %d", pid, testCase.want)
			}
		})
	}
}

// The reason the probe cds instead of handing curl the absolute path: for a real
// UUID that path is past AF_UNIX's sun_path limit, and a connect to it fails
// with ENAMETOOLONG no matter how correct everything else is.
func TestTheAbsoluteSocketPathIsTooLongToConnectTo(t *testing.T) {
	files := filesFor(testUUID)
	if len(files.socket) <= paths.SunPathMax {
		t.Fatalf("socket path is %d bytes, under the %d-byte sun_path limit: the cd dance would be pointless",
			len(files.socket), paths.SunPathMax)
	}
	if len(files.name) >= paths.SunPathMax {
		t.Errorf("relative socket name %q is itself too long", files.name)
	}
	if files.directory+"/"+files.name != files.socket {
		t.Errorf("directory %q + name %q does not reassemble %q", files.directory, files.name, files.socket)
	}
}
