package run

import (
	"context"
	"fmt"
	"strings"
)

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
// THE RENDERING IS BYTE-IDENTICAL TO THE PYTHON, DELIBERATELY, INCLUDING THE
// SINGLE QUOTES AROUND THE URL. Those quotes are not decoration: the sudoers
// allow-list pins this line literally, so a rendering that differs by one
// character is a denied sudo. An earlier version passed the URL through a `{}`
// hole, which is a no-op for a URL made of safe characters and therefore
// emitted it unquoted — the call was refused on every host, and because the
// caller treats a refusal as "the guest declined to shut down", every graceful
// stop silently became a SIGKILL. Nothing surfaced it: the tests stub this
// method above the rendering, so a fully green suite said nothing.
//
// method and apiPath are spliced rather than quoted because that is what
// produces the Python's bytes. They are caller-fixed literals, never user
// input, and they are validated below so that stays true by construction
// rather than by convention.
func (runner *Runner) FirecrackerAPI(
	ctx context.Context, socketDirectory, socketName, method, apiPath, body string,
) error {
	if err := checkSpliced("method", method); err != nil {
		return err
	}
	if err := checkSpliced("api path", apiPath); err != nil {
		return err
	}
	command := fmt.Sprintf(
		"cd {} && curl --fail --silent --show-error --unix-socket {} "+
			"-X %s 'http://localhost%s' -H 'Content-Type: application/json' -d {}",
		method, apiPath,
	)
	rendered, err := Substitute(command, socketDirectory, socketName, body)
	if err != nil {
		return err
	}
	_, err = runner.Run(ctx, "sudo sh -c {}", rendered)
	return err
}

// splicedCharacters is what a method or an API path may contain. Anything else
// would reach the shell unquoted, so it is refused rather than escaped: these
// values are literals chosen by this codebase, and one that is not is a bug to
// be seen rather than a string to be sanitized.
const splicedCharacters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789/-_."

func checkSpliced(what string, value string) error {
	if value == "" {
		return fmt.Errorf("the Firecracker API %s is empty", what)
	}
	if strings.ContainsFunc(value, func(character rune) bool {
		return !strings.ContainsRune(splicedCharacters, character)
	}) {
		return fmt.Errorf("the Firecracker API %s %q is not a plain literal", what, value)
	}
	return nil
}
