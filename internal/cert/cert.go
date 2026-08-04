// Package cert issues (or renews) the wildcard TLS certificate *.<domain> via
// certbot over an ACME DNS-01 challenge, then reports the on-disk PEM paths and
// the validity window.
//
// UNLIKE the other host verbs Boat ports, this one is a CONTROLLER concern: ACME
// issuance lands the PEMs where the control plane can read them to push to the
// proxy fleet, so there is no host to stage onto and the certbot config lives
// under the controller home (~/.atlas/certbot), a sibling of the SSH known_hosts
// dir. The DNS vendor credentials arrive in the environment (AWS_* or POWERDNS_*),
// never in argv — the parameterized-argv trust model, kept for a secret.
//
// The split is the same one lvm.py / networking.py draw: the certbot argv, the
// on-disk layout and the openssl-date parsing are pure functions, unit-testable
// with no certbot and no openssl; the two subprocess calls (certbot, openssl) are
// the only host-touching part, and they go through the one `commands` seam.
//
// Idempotent: certbot renews-or-skips a still-valid lineage (--keep-until-expiring).
//
// Ported from scripts/issue-cert.py and scripts/lib/atlas/certs.py.
package cert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// commands is the host-touching seam: certbot and openssl, and nothing else. The
// credentials file and the fullchain existence check are controller-local
// filesystem operations, done directly (as the Python does with os.*), not through
// this seam — they touch the controller's own home, never a host, and carry a
// secret that must not reach a command line.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
}

var _ commands = (*run.Runner)(nil)

// IssueCertParams is the wildcard issuance request. The parent threads it from the
// API/CLI layer; the DNS credentials it needs travel in the environment, never here.
type IssueCertParams struct {
	Domain           string // the wildcard zone, e.g. blr1.frappe.dev → issues *.blr1.frappe.dev
	ACMEDirectoryURL string // ACME server directory (Let's Encrypt production or staging)
	AccountEmail     string // ACME registration / expiry-notice email
	// DNSAuthenticator is the DNS plugin NAME (e.g. route53). With no CertbotArgs it
	// renders as `--dns-<name>`; "powerdns" additionally writes a credentials file.
	DNSAuthenticator string
	// CertbotArgs is the full certbot authenticator argv from the DNS provider, for
	// providers whose certbot CLI shape differs. Empty keeps the `--dns-<name>` form.
	CertbotArgs []string
}

// IssueCertResult is the typed result the controller records: where the PEMs
// landed and the raw OpenSSL validity strings (e.g. "Jun  8 00:00:00 2026 GMT"),
// which the controller normalizes to its Datetime fields.
type IssueCertResult struct {
	FullchainPath string
	PrivkeyPath   string
	NotBefore     string
	NotAfter      string
}

// IssueCert issues *.<domain> via certbot DNS-01 and returns the PEM paths and
// validity window. See issueCert for the steps; this resolves the controller-local
// layout (~/.atlas) once and delegates, so a test can drive issueCert against a
// temp layout deterministically.
func IssueCert(ctx context.Context, cmd commands, params IssueCertParams) (IssueCertResult, error) {
	layout, err := defaultLayout()
	if err != nil {
		return IssueCertResult{}, err
	}
	return issueCert(ctx, cmd, layout, params)
}

func issueCert(ctx context.Context, cmd commands, layout layout, params IssueCertParams) (IssueCertResult, error) {
	if params.DNSAuthenticator == "powerdns" {
		if err := writePowerDNSCredentials(layout, params.Domain); err != nil {
			return IssueCertResult{}, err
		}
	}

	command, err := layout.certbotCommand(
		params.Domain, params.ACMEDirectoryURL, params.AccountEmail, params.DNSAuthenticator, params.CertbotArgs,
	)
	if err != nil {
		return IssueCertResult{}, err
	}
	if _, err := cmd.Run(ctx, command); err != nil {
		return IssueCertResult{}, err
	}

	fullchain := layout.fullchainPath(params.Domain)
	privkey := layout.privkeyPath(params.Domain)
	if _, err := os.Stat(fullchain); err != nil {
		return IssueCertResult{}, fmt.Errorf("certbot reported success but %s is missing", fullchain)
	}

	dates, err := cmd.Run(ctx, "openssl x509 -noout -dates -in {}", fullchain)
	if err != nil {
		return IssueCertResult{}, err
	}
	notBefore, notAfter, err := parseOpensslDates(dates)
	if err != nil {
		return IssueCertResult{}, err
	}
	return IssueCertResult{
		FullchainPath: fullchain,
		PrivkeyPath:   privkey,
		NotBefore:     notBefore,
		NotAfter:      notAfter,
	}, nil
}

// writePowerDNSCredentials writes the certbot dns-pdns credentials file from the
// environment, 0600, controller-local. The endpoint and API key come from the
// environment (POWERDNS_API_URL / POWERDNS_API_KEY), never argv, so the secret
// stays off the process table — the same reason the Python writes a file rather
// than passing --dns-pdns-api-key.
func writePowerDNSCredentials(layout layout, domain string) error {
	apiURL := os.Getenv("POWERDNS_API_URL")
	apiKey := os.Getenv("POWERDNS_API_KEY")
	serverID := os.Getenv("POWERDNS_SERVER_ID")
	if serverID == "" {
		serverID = "localhost"
	}
	if apiURL == "" || apiKey == "" {
		return fmt.Errorf("POWERDNS_API_URL and POWERDNS_API_KEY are required for PowerDNS DNS-01")
	}
	path := layout.powerdnsCredentialsPath(domain)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(
		"dns_pdns_endpoint = %s\ndns_pdns_api_key = %s\ndns_pdns_server_id = %s\ndns_pdns_disable_notify = false\n",
		apiURL, apiKey, serverID,
	)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	// Enforce 0600 even against a permissive umask — the file carries an API key.
	return os.Chmod(path, 0o600)
}

// parseOpensslDates parses `openssl x509 -noout -dates` output into (notBefore,
// notAfter) as the raw OpenSSL date strings. Errors if either line is missing —
// the controller depends on both to set the certificate's validity window.
func parseOpensslDates(stdout string) (notBefore, notAfter string, err error) {
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "notBefore="):
			notBefore = strings.TrimSpace(strings.TrimPrefix(line, "notBefore="))
		case strings.HasPrefix(line, "notAfter="):
			notAfter = strings.TrimSpace(strings.TrimPrefix(line, "notAfter="))
		}
	}
	if notBefore == "" || notAfter == "" {
		return "", "", fmt.Errorf("could not parse notBefore/notAfter from openssl output: %q", stdout)
	}
	return notBefore, notAfter, nil
}
