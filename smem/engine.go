// Package smem is an embedded agent memory with temporal decay, decision
// provenance and a signed, append-only audit log that can sync between
// replicas.
//
// Every write appends a signed event to the log; all other tables are
// projections of that log and can be rebuilt from it at any time.
package smem

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Engine is a handle on one smem database (one replica).
type Engine struct {
	db      *sql.DB
	path    string
	replica string

	// Now is the clock; tests replace it.
	Now func() time.Time
	// Actor names who is writing. Each actor signs with its own key.
	Actor string
	// Session groups events from one agent session.
	Session string
	// KeyDir holds private signing keys.
	KeyDir string
	// PropagationDepth is how many hops an invalidation marks dependents
	// suspect automatically. Anything further is reported as at-risk by Review.
	PropagationDepth int

	keys map[string]ed25519.PrivateKey
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

// Open opens or creates the database at path.
func Open(path string) (*Engine, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: writes are serialized and a transaction never waits on
	// a second connection from the same process.
	db.SetMaxOpenConns(1)
	e := &Engine{
		db: db, path: path, Now: time.Now, Actor: defaultActor(), Session: os.Getenv("SMEM_SESSION"),
		KeyDir: DefaultKeyDir(), PropagationDepth: 1, keys: map[string]ed25519.PrivateKey{},
	}
	if _, err := db.Exec(baseSchema + viewsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if e.replica, err = e.meta("replica_id"); err != nil {
		db.Close()
		return nil, err
	}
	if e.replica == "" {
		e.replica = newID("rep")
		if err := e.setMeta("replica_id", e.replica); err != nil {
			db.Close()
			return nil, err
		}
	}
	return e, nil
}

func defaultActor() string {
	if a := os.Getenv("SMEM_ACTOR"); a != "" {
		return a
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// Close closes the database.
func (e *Engine) Close() error { return e.db.Close() }

// Replica is this database's replica id.
func (e *Engine) Replica() string { return e.replica }

// Path is the database file path.
func (e *Engine) Path() string { return e.path }

func (e *Engine) meta(key string) (string, error) {
	var v string
	err := e.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (e *Engine) setMeta(key, value string) error {
	_, err := e.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// wtx is a write transaction. Everything inside it must use w.tx: the pool
// has one connection, so touching e.db would deadlock.
type wtx struct {
	e       *Engine
	tx      *sql.Tx
	emitted []Event
}

func (e *Engine) write(fn func(*wtx) error) ([]Event, error) {
	tx, err := e.db.Begin()
	if err != nil {
		return nil, err
	}
	w := &wtx{e: e, tx: tx}
	if err := fn(w); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return w.emitted, nil
}

// signer returns the actor's key, registering it in the log on first use.
func (w *wtx) signer() (ed25519.PrivateKey, string, error) {
	actor := w.e.Actor
	priv, ok := w.e.keys[actor]
	if !ok {
		var err error
		if priv, err = loadOrCreateKey(w.e.KeyDir, actor); err != nil {
			return nil, "", err
		}
		w.e.keys[actor] = priv
	}
	pub := priv.Public().(ed25519.PublicKey)
	kid := keyID(pub)
	var n int
	if err := w.tx.QueryRow(`SELECT COUNT(*) FROM keys WHERE key_id = ?`, kid).Scan(&n); err != nil {
		return nil, "", err
	}
	if n == 0 {
		reg := keyRegistered{KeyID: kid, Actor: actor, PublicKey: base64.StdEncoding.EncodeToString(pub)}
		if _, err := w.emitWith(priv, kid, evKeyRegistered, kid, reg); err != nil {
			return nil, "", err
		}
		// Keys created on this machine are trusted here by default.
		if _, err := w.tx.Exec(`INSERT OR IGNORE INTO key_trust (key_id, trusted, note) VALUES (?, 1, 'local key')`, kid); err != nil {
			return nil, "", err
		}
	}
	return priv, kid, nil
}

func (w *wtx) emit(typ, subject string, payload any) (Event, error) {
	priv, kid, err := w.signer()
	if err != nil {
		return Event{}, err
	}
	return w.emitWith(priv, kid, typ, subject, payload)
}

func (w *wtx) emitWith(priv ed25519.PrivateKey, kid, typ, subject string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	// Redact before hashing: the log is append-only, so a secret that gets
	// in can never be removed.
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(Redact(string(raw)))); err != nil {
		return Event{}, fmt.Errorf("payload after redaction: %w", err)
	}
	ev := Event{Replica: w.e.replica, Actor: w.e.Actor, KeyID: kid, Session: w.e.Session,
		Type: typ, Subject: subject, Payload: buf.Bytes()}
	err = w.tx.QueryRow(`SELECT rseq, hash FROM events WHERE replica = ? ORDER BY rseq DESC LIMIT 1`, w.e.replica).Scan(&ev.RSeq, &ev.Prev)
	if errors.Is(err, sql.ErrNoRows) {
		ev.Prev = genesisHash
	} else if err != nil {
		return Event{}, err
	}
	ev.RSeq++
	var maxAt sql.NullInt64
	if err := w.tx.QueryRow(`SELECT MAX(at) FROM events`).Scan(&maxAt); err != nil {
		return Event{}, err
	}
	ev.At = w.e.Now().UnixMilli()
	if maxAt.Valid && ev.At <= maxAt.Int64 {
		ev.At = maxAt.Int64 + 1
	}
	ev.seal(priv)
	if err := insertEvent(w.tx, &ev); err != nil {
		return Event{}, err
	}
	if err := apply(w.tx, ev); err != nil {
		return Event{}, fmt.Errorf("apply %s: %w", typ, err)
	}
	w.emitted = append(w.emitted, ev)
	return ev, nil
}

// lookup resolves a record id to its kind and status.
func (w *wtx) lookup(id string) (kind, status string, err error) {
	kind, _, _ = strings.Cut(id, "-")
	var q string
	switch kind {
	case "mem":
		q = `SELECT status FROM memories WHERE id = ?`
	case "dec":
		q = `SELECT status FROM decisions WHERE id = ?`
	case "asm":
		q = `SELECT status FROM assumptions WHERE id = ?`
	case "src":
		q = `SELECT '' FROM sources WHERE id = ?`
	case "act":
		q = `SELECT '' FROM actions WHERE id = ?`
	default:
		return "", "", fmt.Errorf("%q is not a memory, decision, assumption, source or action id", id)
	}
	err = w.tx.QueryRow(q, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return kind, "", fmt.Errorf("no such record %s", id)
	}
	return kind, status, err
}

func (w *wtx) require(id, kind string, statuses ...string) error {
	k, st, err := w.lookup(id)
	if err != nil {
		return err
	}
	if k != kind {
		return fmt.Errorf("%s is not a %s", id, kindName[kind])
	}
	if len(statuses) == 0 {
		return nil
	}
	for _, s := range statuses {
		if st == s {
			return nil
		}
	}
	return fmt.Errorf("%s is %s (expected %s)", id, st, strings.Join(statuses, " or "))
}

var kindName = map[string]string{"mem": "memory", "dec": "decision", "asm": "assumption", "src": "source", "act": "action"}

func isRecordID(s string) bool {
	k, rest, ok := strings.Cut(s, "-")
	return ok && rest != "" && kindName[k] != ""
}

func newID(prefix string) string {
	b := make([]byte, 5)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func optTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// queryAll scans every row of a query with scan.
func queryAll[T any](rows *sql.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func selectAll[T any](q querier, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	return queryAll(rows, err, scan)
}
