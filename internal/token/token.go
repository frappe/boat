// Package token holds the bearer token the tunnel listener demands, and the
// hard expiry past which it demands nothing.
//
// The token is a secret Atlas mints per host (spec/33 §12). Rotation is Atlas
// replacing the token file and signalling the daemon to reload it, so the live
// token changes without a restart that would drop the tunnel and every verb on
// it. The hard expiry is the backstop that section requires: a token Atlas has
// not re-minted stays valid only until then, so a leaked-but-unrotated token is
// not good forever — and, because Atlas is the side that re-mints, a partition
// that keeps Atlas from rotating still cannot lock the operator out before the
// expiry, only at it.
//
// Safe by default. A token file that is a bare string carries no expiry and
// never expires — exactly the static token WO-0 shipped. The expiry engages only
// for the JSON form Atlas writes, so a host still on the old file keeps working
// unchanged, and a bug in the expiry path cannot disarm a host that never opted
// into it.
package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// Store holds the current token and serves it to the tunnel listener until a
// reload replaces it or its hard expiry passes.
type Store struct {
	path string
	// now is the clock the expiry is read against, a seam a test replaces to
	// cross an expiry without living through it.
	now func() time.Time

	mutex   sync.RWMutex
	token   string
	expires time.Time // the zero time means never
}

// document is the JSON form of the token file. A file that does not parse as
// this is read as a bare token string that never expires.
type document struct {
	Token         string    `json:"token"`
	HardExpiresAt time.Time `json:"hard_expires_at"`
}

// Open reads the token file into a store. A missing file is not an error: a
// socket-only daemon holds no token and refuses the tunnel by having none to
// match (see Current). Whether an empty token is allowed is the daemon's call,
// not this package's.
func Open(path string) (*Store, error) {
	store := &Store{path: path, now: time.Now}
	if err := store.Reload(); err != nil {
		return nil, err
	}
	return store, nil
}

// Reload re-reads the token file, which is how rotation reaches a running
// daemon: Atlas writes the new file and signals a reload, and the listener
// demands the new token from the next request on. A file gone missing clears the
// token rather than keeping the last one, so a token an operator deleted stops
// being accepted.
func (store *Store) Reload() error {
	token, expires, err := read(store.path)
	if err != nil {
		return err
	}
	store.mutex.Lock()
	store.token, store.expires = token, expires
	store.mutex.Unlock()
	return nil
}

// Current is the token the listener will accept right now: the loaded token,
// unless its hard expiry has passed, in which case it is empty and matches
// nothing. Empty is fail-closed on purpose — a daemon past its token's expiry
// refuses the tunnel rather than trusting a secret Atlas had time to rotate and
// did not.
func (store *Store) Current() string {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	if store.token == "" {
		return ""
	}
	if !store.expires.IsZero() && !store.now().Before(store.expires) {
		return ""
	}
	return store.token
}

// read parses the token file. The JSON form carries an expiry; a bare string is
// the WO-0 static token, which never expires. A file that starts with '{' is
// held to the JSON contract rather than silently accepted as a literal token, so
// a malformed rotation is a loud refusal, not a token nobody can guess.
func read(path string) (string, time.Time, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not read the token file %s: %w", path, err)
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return "", time.Time{}, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var parsed document
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return "", time.Time{}, fmt.Errorf("could not read %s as a token document: %w", path, err)
		}
		return strings.TrimSpace(parsed.Token), parsed.HardExpiresAt, nil
	}
	return trimmed, time.Time{}, nil
}
