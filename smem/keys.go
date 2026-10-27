package smem

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type keyRegistered struct {
	KeyID     string `json:"key_id"`
	Actor     string `json:"actor"`
	PublicKey string `json:"public_key"`
}

func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "key-" + hex.EncodeToString(sum[:8])
}

// DefaultKeyDir is where private keys live: $SMEM_KEY_DIR, or the user config
// dir. Keys are deliberately kept outside the database and any sync remote.
func DefaultKeyDir() string {
	if d := os.Getenv("SMEM_KEY_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "smem", "keys")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// loadOrCreateKey returns the actor's signing key, generating one on first use.
func loadOrCreateKey(dir, actor string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dir, unsafeName.ReplaceAllString(actor, "_")+".ed25519")
	if b, err := os.ReadFile(path); err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("corrupt key file %s", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) { // another process won the race; use its key
			return loadOrCreateKey(dir, actor)
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
		return nil, err
	}
	return priv, nil
}

func decodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad public key")
	}
	return ed25519.PublicKey(b), nil
}

// Key is a registered signing key and the local trust decision about it.
type Key struct {
	KeyID   string `json:"key_id"`
	Actor   string `json:"actor"`
	Replica string `json:"replica"`
	Trusted bool   `json:"trusted"`
	Events  int    `json:"events"`
}

// Keys lists every key that has signed events in this database.
func (e *Engine) Keys() ([]Key, error) {
	rows, err := e.db.Query(`SELECT k.key_id, k.actor, k.replica, COALESCE(t.trusted, 0),
		(SELECT COUNT(*) FROM events ev WHERE ev.key_id = k.key_id)
		FROM keys k LEFT JOIN key_trust t ON t.key_id = k.key_id ORDER BY k.actor, k.key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.KeyID, &k.Actor, &k.Replica, &k.Trusted, &k.Events); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetTrust records a local trust decision. Trust is policy, like ssh
// known_hosts: it is not an event and is never replicated.
func (e *Engine) SetTrust(keyID string, trusted bool, note string) error {
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM keys WHERE key_id = ?`, keyID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown key %s", keyID)
	}
	_, err := e.db.Exec(`INSERT INTO key_trust (key_id, trusted, note) VALUES (?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET trusted = excluded.trusted, note = excluded.note`, keyID, trusted, note)
	return err
}
