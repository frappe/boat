package run

import "context"

// FirecrackerAPI calls the Firecracker API over one VM's jailed unix socket.
//
// The socket's absolute path is longer than AF_UNIX's 108-byte sun_path limit,
// so the call cds into the socket's directory and addresses the socket by its
// short relative name. That cd has to happen as root, because the directory is
// 0700 and owned by the per-VM uid, which is why the whole line runs under one
// `sudo sh -c`: it renders to the argv ["sudo", "sh", "-c", "<the whole quoted
// line>"], so the cd and the curl share one shell and one working directory.
//
// --fail makes a 4xx or 5xx exit non-zero, so a state change Firecracker
// refused surfaces as a failed operation instead of a silent success.
//
// Unlike the Python, method and apiPath are quoted parameters rather than text
// spliced into the template. They are caller-fixed literals today (PATCH /vm,
// PUT /snapshot/create), and for such values quoting is a no-op — but the Go
// signature makes them arguments, and an argument that is not quoted is exactly
// the hole this package exists to close.
func (runner *Runner) FirecrackerAPI(
	ctx context.Context, socketDirectory, socketName, method, apiPath, body string,
) error {
	command := "cd {} && curl --fail --silent --show-error --unix-socket {} " +
		"-X {} {} -H 'Content-Type: application/json' -d {}"
	rendered, err := Substitute(command, socketDirectory, socketName, method, "http://localhost"+apiPath, body)
	if err != nil {
		return err
	}
	_, err = runner.Run(ctx, "sudo sh -c {}", rendered)
	return err
}
