package datum

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
)

// TokenSet holds the datum bearer tokens Atlas ships to this host: one for the
// host's own metrics (resource_id = the Server name) and one per VM keyed by VM
// UUID (resource_id = the VM name). It mirrors internal/token: a missing file is
// empty, not an error, and Reload re-reads it so Atlas can rotate on SIGHUP
// without a daemon restart.
type TokenSet struct {
	path  string
	mutex sync.RWMutex
	host  string
	vms   map[string]string
}

// document is the JSON shape of the token file:
// {"host":"<jwt>","vms":{"<uuid>":"<jwt>", ...}}
type document struct {
	Host string            `json:"host"`
	VMs  map[string]string `json:"vms"`
}

// Open reads the token file into a set. A missing file yields an empty set.
func Open(path string) (*TokenSet, error) {
	set := &TokenSet{path: path, vms: map[string]string{}}
	if err := set.Reload(); err != nil {
		return nil, err
	}
	return set, nil
}

// Reload re-reads the file. A file gone missing clears the tokens (fail-closed)
// rather than keeping the last ones. A non-empty file that does not parse as the
// JSON document is a loud error, not a silently-accepted literal.
func (set *TokenSet) Reload() error {
	content, err := os.ReadFile(set.path)
	if errors.Is(err, fs.ErrNotExist) {
		set.replace("", map[string]string{})
		return nil
	}
	if err != nil {
		return fmt.Errorf("datum: read token file %s: %w", set.path, err)
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		set.replace("", map[string]string{})
		return nil
	}
	var parsed document
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return fmt.Errorf("datum: parse token file %s: %w", set.path, err)
	}
	vms := parsed.VMs
	if vms == nil {
		vms = map[string]string{}
	}
	set.replace(strings.TrimSpace(parsed.Host), vms)
	return nil
}

func (set *TokenSet) replace(host string, vms map[string]string) {
	set.mutex.Lock()
	set.host, set.vms = host, vms
	set.mutex.Unlock()
}

// HostToken is the token for this host's own (host_*) samples, or "" if none.
func (set *TokenSet) HostToken() string {
	set.mutex.RLock()
	defer set.mutex.RUnlock()
	return set.host
}

// TokenFor is the token for a VM's samples by its UUID. A VM with its own token
// uses it; otherwise it falls back to the host token, so a single-token deployment
// ({"host":"<jwt>","vms":{}}) pushes every VM's samples under that one token — the
// VMs are distinguished by a vm=<uuid> label, not by resource_id. Returns "" only
// when neither a per-VM token nor a host token is held.
func (set *TokenSet) TokenFor(uuid string) string {
	set.mutex.RLock()
	defer set.mutex.RUnlock()
	if token, ok := set.vms[uuid]; ok {
		return token
	}
	return set.host
}
