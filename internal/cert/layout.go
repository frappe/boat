package cert

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// layout is the controller-local certbot path layout, rooted at ~/.atlas. It is a
// value (not a package of globals) so a test can construct one over a temp
// directory and get deterministic paths, the way certs.py is exercised with a
// fixed home in test_certs.py.
type layout struct {
	// atlasHome is ~/.atlas, matching the SSH transport's known_hosts parent so
	// controller state is colocated.
	atlasHome string
}

// defaultLayout resolves ~/.atlas from the controller user's home.
func defaultLayout() (layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return layout{}, err
	}
	return layout{atlasHome: filepath.Join(home, ".atlas")}, nil
}

// certbotExecutable is the certbot on PATH. Atlas resolved a venv-adjacent certbot
// (dirname(sys.executable)/certbot); Boat has no venv — the venv constants name the
// Python interpreter Boat exists to retire — so it names the binary directly, a
// controller-host dependency like openssl.
func certbotExecutable() string { return "certbot" }

// certbotConfigDir is certbot's --config-dir for this domain — per-domain so
// accounts and renewal state never collide across regions.
func (layout layout) certbotConfigDir(domain string) string {
	return filepath.Join(layout.atlasHome, "certbot", domain)
}

// liveDir is where certbot writes the live symlinks for *.<domain>. certbot names
// the lineage after the first -d with the leading `*.` stripped, i.e. <domain>.
func (layout layout) liveDir(domain string) string {
	return filepath.Join(layout.certbotConfigDir(domain), "live", domain)
}

func (layout layout) powerdnsCredentialsPath(domain string) string {
	return filepath.Join(layout.certbotConfigDir(domain), "powerdns.ini")
}

func (layout layout) fullchainPath(domain string) string {
	return filepath.Join(layout.liveDir(domain), "fullchain.pem")
}

func (layout layout) privkeyPath(domain string) string {
	return filepath.Join(layout.liveDir(domain), "privkey.pem")
}

// certbotArgsFor is the DNS authenticator argv when the provider supplies none:
// the plain `--dns-<name>` form, or the dns-pdns credentials shape for PowerDNS.
func (layout layout) certbotArgsFor(domain, dnsAuthenticator string) []string {
	if dnsAuthenticator == "powerdns" {
		return []string{
			"--authenticator", "dns-pdns",
			"--dns-pdns-credentials", layout.powerdnsCredentialsPath(domain),
		}
	}
	return []string{"--dns-" + dnsAuthenticator}
}

// certbotCommand is the full certbot command line to issue (or renew) *.<domain>
// non-interactively over DNS-01, rendered as a single shell-quoted string for the
// `commands` seam to run. When certbotArgs is empty the authenticator is rendered
// as `--dns-<name>`; a provider with a different CLI shape passes explicit args.
// Credentials travel via the environment or a controller-local 0600 file, never as
// secret argv values. Idempotent: certbot renews-or-skips a still-valid lineage.
//
// The head/tail go through run.Substitute (each hole shell-quoted), and the DNS
// args are run.Quote'd individually, so the finished line re-splits into exactly
// the argv the Python's certs.certbot_command produces.
func (layout layout) certbotCommand(
	domain, acmeDirectoryURL, accountEmail, dnsAuthenticator string, certbotArgs []string,
) (string, error) {
	config := layout.certbotConfigDir(domain)
	dnsArgs := certbotArgs
	if len(dnsArgs) == 0 {
		dnsArgs = layout.certbotArgsFor(domain, dnsAuthenticator)
	}
	quoted := make([]string, len(dnsArgs))
	for index, arg := range dnsArgs {
		quoted[index] = run.Quote(arg)
	}

	head, err := run.Substitute(
		"{} certonly --non-interactive --agree-tos -m {} --server {}",
		certbotExecutable(), accountEmail, acmeDirectoryURL,
	)
	if err != nil {
		return "", err
	}
	tail, err := run.Substitute(
		"-d {} --config-dir {} --work-dir {} --logs-dir {} --keep-until-expiring",
		"*."+domain, config, filepath.Join(config, "work"), filepath.Join(config, "logs"),
	)
	if err != nil {
		return "", err
	}
	return head + " " + strings.Join(quoted, " ") + " " + tail, nil
}
