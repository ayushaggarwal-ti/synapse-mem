package smem

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Event types. Every state change in smem is one of these.
const (
	evKeyRegistered      = "key.registered"
	evMemoryAsserted     = "memory.asserted"
	evMemoryInvalidated  = "memory.invalidated"
	evMemoryRetracted    = "memory.retracted"
	evMemorySuspected    = "memory.suspected"
	evMemoryReconfirmed  = "memory.reconfirmed"
	evMemorySignal       = "memory.signal"
	evMemoryArchived     = "memory.archived"
	evRecall             = "recall"
	evSourceCaptured     = "source.captured"
	evDecisionMade       = "decision.made"
	evDecisionOutcome    = "decision.outcome"
	evDecisionApproved   = "decision.approved"
	evDecisionSuspected  = "decision.suspected"
	evDecisionReaffirmed = "decision.reaffirmed"
	evDecisionReverted   = "decision.reverted"
	evAssumptionBroken   = "assumption.broken"
	evAssumptionHeld     = "assumption.held"
	evActionRecorded     = "action.recorded"
)

// Event is one signed, hash-chained entry in a replica's feed.
//
// Each replica (one database) appends to its own chain: RSeq increases by one
// and Prev is the hash of the replica's previous event. Hash covers every
// field except Hash, Sig and ID; Sig is the actor's Ed25519 signature over
// that hash. The merged log is ordered by (At, Replica, RSeq), and At is kept
// strictly greater than every event the writer had seen, so the order
// respects causality across replicas.
type Event struct {
	Seq     int64           `json:"-"`
	ID      string          `json:"id"`
	Replica string          `json:"replica"`
	RSeq    int64           `json:"rseq"`
	At      int64           `json:"at"`
	Actor   string          `json:"actor"`
	KeyID   string          `json:"key_id"`
	Session string          `json:"session,omitempty"`
	Type    string          `json:"type"`
	Subject string          `json:"subject,omitempty"`
	Payload json.RawMessage `json:"payload"`
	Prev    string          `json:"prev"`
	Hash    string          `json:"hash"`
	Sig     string          `json:"sig"`
}

func (ev *Event) digest() []byte {
	b, _ := json.Marshal(struct {
		Replica string          `json:"replica"`
		RSeq    int64           `json:"rseq"`
		At      int64           `json:"at"`
		Actor   string          `json:"actor"`
		KeyID   string          `json:"key_id"`
		Session string          `json:"session"`
		Type    string          `json:"type"`
		Subject string          `json:"subject"`
		Payload json.RawMessage `json:"payload"`
		Prev    string          `json:"prev"`
	}{ev.Replica, ev.RSeq, ev.At, ev.Actor, ev.KeyID, ev.Session, ev.Type, ev.Subject, ev.Payload, ev.Prev})
	sum := sha256.Sum256(b)
	return sum[:]
}

func (ev *Event) seal(priv ed25519.PrivateKey) {
	d := ev.digest()
	ev.Hash = hex.EncodeToString(d)
	ev.ID = "evt-" + ev.Hash[:16]
	ev.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, d))
}

// check recomputes the hash and verifies the signature against pub.
func (ev *Event) check(pub ed25519.PublicKey) error {
	d := ev.digest()
	if hex.EncodeToString(d) != ev.Hash {
		return errors.New("hash mismatch: event content was modified")
	}
	if ev.ID != "evt-"+ev.Hash[:16] {
		return errors.New("id does not match hash")
	}
	sig, err := base64.StdEncoding.DecodeString(ev.Sig)
	if err != nil || !ed25519.Verify(pub, d, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// Time is the recorded (transaction) time of the event.
func (ev Event) Time() time.Time { return fromMs(ev.At) }

func (ev Event) String() string {
	return fmt.Sprintf("%s %s/%d %s %s %s", ev.Time().Local().Format("2006-01-02 15:04:05"), ev.Replica, ev.RSeq, ev.Actor, ev.Type, ev.Subject)
}

// orderLess is the total order used to project the merged log.
func orderLess(a, b Event) bool {
	if a.At != b.At {
		return a.At < b.At
	}
	if a.Replica != b.Replica {
		return a.Replica < b.Replica
	}
	return a.RSeq < b.RSeq
}

const eventCols = `seq, id, replica, rseq, at, actor, key_id, session, type, subject, payload, prev, hash, sig`

type scanner interface{ Scan(...any) error }

func scanEvent(s scanner) (Event, error) {
	var ev Event
	var payload string
	err := s.Scan(&ev.Seq, &ev.ID, &ev.Replica, &ev.RSeq, &ev.At, &ev.Actor, &ev.KeyID, &ev.Session,
		&ev.Type, &ev.Subject, &payload, &ev.Prev, &ev.Hash, &ev.Sig)
	ev.Payload = json.RawMessage(payload)
	return ev, err
}

func insertEvent(x execer, ev *Event) error {
	res, err := x.Exec(`INSERT INTO events (id, replica, rseq, at, actor, key_id, session, type, subject, payload, prev, hash, sig)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Replica, ev.RSeq, ev.At, ev.Actor, ev.KeyID, ev.Session, ev.Type, ev.Subject,
		string(ev.Payload), ev.Prev, ev.Hash, ev.Sig)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("replica %s already has an event at rseq %d (fork): %w", ev.Replica, ev.RSeq, err)
		}
		return err
	}
	ev.Seq, _ = res.LastInsertId()
	return nil
}
