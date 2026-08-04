// Unit tests for the issue-cert helpers — certbot argv construction, the on-disk
// layout, openssl-date parsing, the PowerDNS credentials file — plus a recorder
// golden over the two subprocess calls (certbot, openssl). Nothing here needs
// certbot, openssl or a controller. Mirrors scripts/lib/atlas/test_certs.py.

package cert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/run"
)

const (
	testDomain = "blr1.frappe.dev"
	testACME   = "https://acme-staging-v02.api.letsencrypt.org/directory"
	testEmail  = "ops@frappe.dev"
)

func testLayout(t *testing.T) layout {
	t.Helper()
	return layout{atlasHome: filepath.Join(t.TempDir(), ".atlas")}
}

// argv splits a certbot command the way the real Runner would, so the tests read
// tokens rather than a quoted line — the shape test_certs.py asserts.
func argv(t *testing.T, command string) []string {
	t.Helper()
	split, err := run.Split(command)
	if err != nil {
		t.Fatalf("splitting %q: %v", command, err)
	}
	return split
}

func indexOf(t *testing.T, tokens []string, want string) int {
	t.Helper()
	for index, token := range tokens {
		if token == want {
			return index
		}
	}
	t.Fatalf("token %q not found in %v", want, tokens)
	return -1
}

func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}

func TestCertbotCommandIssuesTheWildcard(t *testing.T) {
	command, err := testLayout(t).certbotCommand(testDomain, testACME, testEmail, "route53", nil)
	if err != nil {
		t.Fatalf("certbotCommand: %v", err)
	}
	tokens := argv(t, command)

	if tokens[0] != "certbot" || tokens[1] != "certonly" {
		t.Errorf("head = %v, want certbot certonly", tokens[:2])
	}
	if indexOf(t, tokens, "--non-interactive") < 0 {
		t.Error("missing --non-interactive")
	}
	// The cert is the wildcard *.<domain>, requested via -d.
	if got := tokens[indexOf(t, tokens, "-d")+1]; got != "*."+testDomain {
		t.Errorf("-d = %q, want *.%s", got, testDomain)
	}
	// The plain authenticator name becomes the --dns-<name> flag.
	if indexOf(t, tokens, "--dns-route53") < 0 {
		t.Error("missing --dns-route53")
	}
	// Account email and ACME server are passed.
	if got := tokens[indexOf(t, tokens, "-m")+1]; got != testEmail {
		t.Errorf("-m = %q, want %s", got, testEmail)
	}
	if got := tokens[indexOf(t, tokens, "--server")+1]; got != testACME {
		t.Errorf("--server = %q, want %s", got, testACME)
	}
	// The config dir is per-domain under ~/.atlas.
	config := tokens[indexOf(t, tokens, "--config-dir")+1]
	if !strings.HasSuffix(config, filepath.Join(".atlas", "certbot", testDomain)) {
		t.Errorf("--config-dir = %q, want it under .atlas/certbot/%s", config, testDomain)
	}
	// No credentials in argv — they travel via the environment.
	for _, token := range tokens {
		if strings.Contains(token, "AWS") || strings.Contains(strings.ToLower(token), "secret") {
			t.Errorf("credential-shaped token in argv: %q", token)
		}
	}
}

func TestCertbotCommandUsesProviderSuppliedArgs(t *testing.T) {
	layout := testLayout(t)
	credentials := layout.powerdnsCredentialsPath(testDomain)
	command, err := layout.certbotCommand(testDomain, testACME, testEmail, "powerdns", []string{
		"--authenticator", "dns-pdns", "--dns-pdns-credentials", credentials,
	})
	if err != nil {
		t.Fatalf("certbotCommand: %v", err)
	}
	tokens := argv(t, command)
	if containsToken(tokens, "--dns-powerdns") {
		t.Error("provider args should replace the --dns-<name> rendering")
	}
	if got := tokens[indexOf(t, tokens, "--authenticator")+1]; got != "dns-pdns" {
		t.Errorf("--authenticator = %q, want dns-pdns", got)
	}
	if got := tokens[indexOf(t, tokens, "--dns-pdns-credentials")+1]; got != credentials {
		t.Errorf("--dns-pdns-credentials = %q, want %q", got, credentials)
	}
}

func TestCertPaths(t *testing.T) {
	layout := testLayout(t)
	if !filepath.IsAbs(layout.powerdnsCredentialsPath(testDomain)) {
		t.Error("powerdns credentials path is not absolute")
	}
	if !strings.HasSuffix(layout.powerdnsCredentialsPath(testDomain), filepath.Join(testDomain, "powerdns.ini")) {
		t.Error("powerdns credentials path is not under the domain dir")
	}
	if !strings.HasSuffix(layout.fullchainPath(testDomain), filepath.Join("live", testDomain, "fullchain.pem")) {
		t.Error("fullchain path is not under the domain live dir")
	}
	if !strings.HasSuffix(layout.privkeyPath(testDomain), filepath.Join("live", testDomain, "privkey.pem")) {
		t.Error("privkey path is not under the domain live dir")
	}
}

func TestParseOpensslDates(t *testing.T) {
	notBefore, notAfter, err := parseOpensslDates(
		"notBefore=Jun  8 00:00:00 2026 GMT\nnotAfter=Sep  6 23:59:59 2026 GMT\n",
	)
	if err != nil {
		t.Fatalf("parseOpensslDates: %v", err)
	}
	if notBefore != "Jun  8 00:00:00 2026 GMT" || notAfter != "Sep  6 23:59:59 2026 GMT" {
		t.Errorf("dates = (%q, %q)", notBefore, notAfter)
	}
	if _, _, err := parseOpensslDates("some unrelated output\n"); err == nil {
		t.Error("expected an error when the dates are missing")
	}
}

func TestWritePowerDNSCredentials(t *testing.T) {
	layout := testLayout(t)
	t.Setenv("POWERDNS_API_URL", "https://pdns.example/api/v1")
	t.Setenv("POWERDNS_API_KEY", "s3cr3t")
	t.Setenv("POWERDNS_SERVER_ID", "")

	if err := writePowerDNSCredentials(layout, testDomain); err != nil {
		t.Fatalf("writePowerDNSCredentials: %v", err)
	}
	path := layout.powerdnsCredentialsPath(testDomain)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading credentials: %v", err)
	}
	want := "dns_pdns_endpoint = https://pdns.example/api/v1\n" +
		"dns_pdns_api_key = s3cr3t\n" +
		"dns_pdns_server_id = localhost\n" + // empty env defaults to localhost
		"dns_pdns_disable_notify = false\n"
	if string(content) != want {
		t.Errorf("credentials content:\ngot:\n%s\nwant:\n%s", content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWritePowerDNSCredentialsRequiresEnv(t *testing.T) {
	t.Setenv("POWERDNS_API_URL", "")
	t.Setenv("POWERDNS_API_KEY", "")
	if err := writePowerDNSCredentials(testLayout(t), testDomain); err == nil {
		t.Error("expected an error when the PowerDNS env is unset")
	}
}

// fakeCommands records the two subprocess calls and scripts their output.
type fakeCommands struct {
	outputs map[string]string
	failing map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, failing: map[string]bool{}}
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := renderCert(template, parameters...)
	fake.trace = append(fake.trace, command)
	if fake.failing[command] {
		return "", errors.New("command failed")
	}
	return fake.outputs[command], nil
}

func renderCert(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			builder.WriteString(parameters[index].(string))
		}
	}
	return builder.String()
}

// The recorder golden: certbot runs first, then openssl on the fullchain, and the
// parsed validity window is returned. certbot does not create the PEM here, so the
// test lays it down under the temp layout first — the same fullchain the existence
// check and openssl read.
func TestIssueCertRunsCertbotThenOpenssl(t *testing.T) {
	layout := testLayout(t)
	fullchain := layout.fullchainPath(testDomain)
	if err := os.MkdirAll(filepath.Dir(fullchain), 0o755); err != nil {
		t.Fatalf("staging live dir: %v", err)
	}
	if err := os.WriteFile(fullchain, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatalf("staging fullchain: %v", err)
	}

	fake := newFakeCommands()
	fake.outputs["openssl x509 -noout -dates -in "+fullchain] =
		"notBefore=Jun  8 00:00:00 2026 GMT\nnotAfter=Sep  6 23:59:59 2026 GMT\n"

	result, err := issueCert(context.Background(), fake, layout, IssueCertParams{
		Domain:           testDomain,
		ACMEDirectoryURL: testACME,
		AccountEmail:     testEmail,
		DNSAuthenticator: "route53",
	})
	if err != nil {
		t.Fatalf("issueCert: %v", err)
	}

	if len(fake.trace) != 2 {
		t.Fatalf("expected certbot then openssl, got:\n  %s", strings.Join(fake.trace, "\n  "))
	}
	certbot := argv(t, fake.trace[0])
	if certbot[0] != "certbot" || certbot[1] != "certonly" {
		t.Errorf("first command was not certbot certonly: %v", certbot[:2])
	}
	if got := certbot[indexOf(t, certbot, "-d")+1]; got != "*."+testDomain {
		t.Errorf("certbot -d = %q", got)
	}
	if fake.trace[1] != "openssl x509 -noout -dates -in "+fullchain {
		t.Errorf("second command = %q", fake.trace[1])
	}

	if result.FullchainPath != fullchain || result.PrivkeyPath != layout.privkeyPath(testDomain) {
		t.Errorf("result paths = %+v", result)
	}
	if result.NotBefore != "Jun  8 00:00:00 2026 GMT" || result.NotAfter != "Sep  6 23:59:59 2026 GMT" {
		t.Errorf("result validity = (%q, %q)", result.NotBefore, result.NotAfter)
	}
}

// certbot "succeeding" without leaving a fullchain is a loud failure, and openssl
// never runs.
func TestIssueCertFailsWhenFullchainMissing(t *testing.T) {
	layout := testLayout(t)
	fake := newFakeCommands()
	if _, err := issueCert(context.Background(), fake, layout, IssueCertParams{
		Domain:           testDomain,
		ACMEDirectoryURL: testACME,
		AccountEmail:     testEmail,
		DNSAuthenticator: "route53",
	}); err == nil {
		t.Fatal("expected an error when the fullchain is missing")
	}
	for _, command := range fake.trace {
		if strings.HasPrefix(command, "openssl") {
			t.Error("openssl ran even though the fullchain was missing")
		}
	}
}
