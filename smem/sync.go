package smem

import (
	"bufio"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ImportReport describes what an import accepted and refused.
type ImportReport struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"` // already present
	Rejected []string `json:"rejected"`
	Rebuilt  bool     `json:"rebuilt"` // projections were replayed because events arrived out of order
	Flags    []Flag   `json:"flags"`   // records marked suspect locally as a result
}

// Import merges events from other replicas. Each feed is verified on its
// own: contiguous rseq, intact chain, valid hash and signature. A feed is
// accepted up to its first bad event; nothing after a bad event is trusted.
// A different event at an rseq we already have is a fork and is refused.
func (e *Engine) Import(evs []Event) (*ImportReport, error) {
	rep := &ImportReport{}
	emitted, err := e.write(func(w *wtx) error {
		keys, err := w.publicKeys()
		if err != nil {
			return err
		}
		// Registrations can arrive in the same batch as the events they sign.
		for _, ev := range evs {
			if ev.Type != evKeyRegistered {
				continue
			}
			var p keyRegistered
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			if pub, err := decodePublicKey(p.PublicKey); err == nil && keyID(pub) == p.KeyID && ev.KeyID == p.KeyID {
				if _, ok := keys[p.KeyID]; !ok {
					keys[p.KeyID] = keyInfo{pub: pub, actor: p.Actor}
				}
			}
		}

		var last Event
		haveLast := w.tx.QueryRow(`SELECT `+eventCols+` FROM events ORDER BY at DESC, replica DESC, rseq DESC LIMIT 1`).Scan(
			&last.Seq, &last.ID, &last.Replica, &last.RSeq, &last.At, &last.Actor, &last.KeyID, &last.Session,
			&last.Type, &last.Subject, new(string), &last.Prev, &last.Hash, &last.Sig) == nil

		feeds := map[string][]Event{}
		for _, ev := range evs {
			feeds[ev.Replica] = append(feeds[ev.Replica], ev)
		}
		replicas := make([]string, 0, len(feeds))
		for r := range feeds {
			replicas = append(replicas, r)
		}
		sort.Strings(replicas)

		var accepted []Event
		for _, r := range replicas {
			feed := feeds[r]
			if r == w.e.replica {
				rep.Skipped += len(feed)
				continue
			}
			sort.Slice(feed, func(i, j int) bool { return feed[i].RSeq < feed[j].RSeq })
			headSeq, headHash, err := w.head(r)
			if err != nil {
				return err
			}
			for _, ev := range feed {
				reject := func(why string) { rep.Rejected = append(rep.Rejected, fmt.Sprintf("%s/%d: %s", r, ev.RSeq, why)) }
				if ev.RSeq <= headSeq {
					var have string
					if err := w.tx.QueryRow(`SELECT hash FROM events WHERE replica = ? AND rseq = ?`, r, ev.RSeq).Scan(&have); err != nil {
						return err
					}
					if have != ev.Hash {
						reject("fork: differs from the event we already have at this position")
						break
					}
					rep.Skipped++
					continue
				}
				if ev.RSeq != headSeq+1 {
					reject(fmt.Sprintf("gap: expected rseq %d", headSeq+1))
					break
				}
				if ev.Prev != headHash {
					reject("chain broken: prev does not match the preceding event")
					break
				}
				k, ok := keys[ev.KeyID]
				if !ok {
					reject("signed by unknown key " + ev.KeyID)
					break
				}
				if k.actor != ev.Actor {
					reject(fmt.Sprintf("key %s belongs to %s, not %s", ev.KeyID, k.actor, ev.Actor))
					break
				}
				if err := ev.check(k.pub); err != nil {
					reject(err.Error())
					break
				}
				if err := insertEvent(w.tx, &ev); err != nil {
					reject(err.Error())
					break
				}
				accepted = append(accepted, ev)
				headSeq, headHash = ev.RSeq, ev.Hash
			}
		}
		if len(accepted) == 0 {
			return nil
		}
		sort.Slice(accepted, func(i, j int) bool { return orderLess(accepted[i], accepted[j]) })
		if haveLast && orderLess(accepted[0], last) {
			// Something arrived that belongs before what we have already
			// projected; replay everything in order.
			if err := rebuildViews(w.tx); err != nil {
				return err
			}
			rep.Rebuilt = true
		} else {
			for _, ev := range accepted {
				if err := apply(w.tx, ev); err != nil {
					return fmt.Errorf("apply %s: %w", ev.ID, err)
				}
			}
		}
		rep.Accepted = len(accepted)
		// The remote writer could not see our local dependents, so propagate
		// its invalidations here too. Propagation only touches active
		// records, so causes it already handled are no-ops.
		for _, ev := range accepted {
			if root := propagationRoot(ev); root != "" {
				if err := w.propagate(root, "via "+ev.Replica+": "+ev.Type); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rep.Flags = flagsFrom(emitted)
	return rep, nil
}

type keyInfo struct {
	pub   ed25519.PublicKey
	actor string
}

func (w *wtx) publicKeys() (map[string]keyInfo, error) {
	rows, err := w.tx.Query(`SELECT key_id, actor, public_key FROM keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]keyInfo{}
	for rows.Next() {
		var id, actor, pk string
		if err := rows.Scan(&id, &actor, &pk); err != nil {
			return nil, err
		}
		if pub, err := decodePublicKey(pk); err == nil {
			out[id] = keyInfo{pub: pub, actor: actor}
		}
	}
	return out, rows.Err()
}

func (w *wtx) head(replica string) (int64, string, error) {
	var seq int64
	var hash string
	err := w.tx.QueryRow(`SELECT rseq, hash FROM events WHERE replica = ? ORDER BY rseq DESC LIMIT 1`, replica).Scan(&seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, genesisHash, nil
	}
	return seq, hash, err
}

// propagationRoot returns the record whose change should invalidate its
// dependents, if ev is such a change.
func propagationRoot(ev Event) string {
	switch ev.Type {
	case evMemoryInvalidated, evMemoryRetracted, evAssumptionBroken, evDecisionReverted:
		var p statusChange
		json.Unmarshal(ev.Payload, &p)
		return p.ID
	case evMemoryAsserted, evDecisionMade:
		var p struct {
			Supersedes string `json:"supersedes"`
		}
		json.Unmarshal(ev.Payload, &p)
		return p.Supersedes
	}
	return ""
}

// Export writes events as JSON lines. With replica set, only that feed.
func (e *Engine) Export(w io.Writer, replica string) (int, error) {
	q, args := `ORDER BY replica, rseq`, []any{}
	if replica != "" {
		q, args = `WHERE replica = ? ORDER BY rseq`, []any{replica}
	}
	evs, err := e.loadEvents(q, args...)
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(w)
	for _, ev := range evs {
		if err := enc.Encode(ev); err != nil {
			return 0, err
		}
	}
	return len(evs), nil
}

// ReadEvents parses JSON lines written by Export.
func ReadEvents(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var out []Event
	for line := 1; sc.Scan(); line++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// DirRemote is a sync remote that is just a directory: one append-only
// JSONL file per replica under feeds/. Each file has exactly one writer
// (its replica), so it works over a shared disk, Dropbox or a git repo.
// Readers verify everything; the directory itself is not trusted.
type DirRemote struct{ Dir string }

func (r DirRemote) feedPath(replica string) string {
	return filepath.Join(r.Dir, "feeds", replica+".jsonl")
}

func (r DirRemote) readFeed(path string) ([]Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadEvents(f)
}

// Push appends this replica's new events to its feed in the remote.
func (e *Engine) Push(r DirRemote) (int, error) {
	remote, err := r.readFeed(r.feedPath(e.replica))
	if err != nil {
		return 0, err
	}
	n := int64(len(remote))
	if n > 0 {
		var have string
		err := e.db.QueryRow(`SELECT hash FROM events WHERE replica = ? AND rseq = ?`, e.replica, n).Scan(&have)
		if err != nil || have != remote[n-1].Hash || remote[n-1].RSeq != n {
			return 0, fmt.Errorf("remote feed for %s does not match the local log; refusing to push", e.replica)
		}
	}
	evs, err := e.loadEvents(`WHERE replica = ? AND rseq > ? ORDER BY rseq`, e.replica, n)
	if err != nil || len(evs) == 0 {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(r.feedPath(e.replica)), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(r.feedPath(e.replica), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(f)
	for _, ev := range evs {
		if err := enc.Encode(ev); err != nil {
			f.Close()
			return 0, err
		}
	}
	return len(evs), f.Close()
}

// Pull imports every other replica's feed from the remote.
func (e *Engine) Pull(r DirRemote) (*ImportReport, error) {
	paths, err := filepath.Glob(filepath.Join(r.Dir, "feeds", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var all []Event
	for _, p := range paths {
		if strings.TrimSuffix(filepath.Base(p), ".jsonl") == e.replica {
			continue
		}
		evs, err := r.readFeed(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, evs...)
	}
	return e.Import(all)
}

// Sync pulls, then pushes, so that suspect flags raised locally by the pull
// reach the remote in the same round.
func (e *Engine) Sync(r DirRemote) (*ImportReport, int, error) {
	rep, err := e.Pull(r)
	if err != nil {
		return nil, 0, err
	}
	n, err := e.Push(r)
	return rep, n, err
}

// AddRemote saves a named directory remote. Remotes are local config.
func (e *Engine) AddRemote(name, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(abs, "feeds"), 0o755); err != nil {
		return err
	}
	return e.setMeta("remote."+name, abs)
}

// Remotes lists configured remotes by name.
func (e *Engine) Remotes() (map[string]string, error) {
	rows, err := e.db.Query(`SELECT key, value FROM meta WHERE key LIKE 'remote.%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(k, "remote.")] = v
	}
	return out, rows.Err()
}
