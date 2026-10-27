package smem

import (
	"time"
)

// ReviewItem is one thing that needs a human or agent to look at it.
type ReviewItem struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Status  string    `json:"status"`
	Summary string    `json:"summary"`
	Cause   string    `json:"cause,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Via     string    `json:"via,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

// Review is the queue of records whose basis has changed.
type Review struct {
	// Suspect records were flagged directly by propagation.
	Suspect []ReviewItem `json:"suspect"`
	// AtRisk records are further downstream of a suspect record and were
	// deliberately not flagged automatically.
	AtRisk []ReviewItem `json:"at_risk"`
	// Conflicts are records with more than one live successor, usually
	// because two replicas superseded the same thing independently.
	Conflicts []ReviewItem `json:"conflicts"`
	// FactCheck lists memories whose latest fact-check needs a person.
	FactCheck []ReviewItem `json:"factcheck"`
	// UntrustedKeys signed events here but are not trusted locally.
	UntrustedKeys []Key `json:"untrusted_keys"`
	// Judges lists judges with failed calls in the last 24 hours.
	Judges []JudgeHealth `json:"judges,omitempty"`
}

func scanReviewItem(s scanner) (ReviewItem, error) {
	var it ReviewItem
	var at int64
	err := s.Scan(&it.ID, &it.Kind, &it.Status, &it.Summary, &it.Cause, &it.Reason, &it.Via, &at)
	if at > 0 {
		it.At = fromMs(at)
	}
	return it, err
}

const latestSuspicion = `LEFT JOIN status_log s ON s.rowid =
	(SELECT rowid FROM status_log WHERE subject = x.id AND status = 'suspected' ORDER BY at DESC LIMIT 1)`

const suspectQuery = `
	SELECT x.id, 'decision', x.status, x.question || ' → ' || x.choice, COALESCE(s.cause, ''), COALESCE(s.reason, ''), '', COALESCE(s.at, 0)
		FROM decisions x ` + latestSuspicion + ` WHERE x.status = 'suspect'
	UNION ALL
	SELECT x.id, 'memory', x.status, x.content, COALESCE(s.cause, ''), COALESCE(s.reason, ''), '', COALESCE(s.at, 0)
		FROM memories x ` + latestSuspicion + ` WHERE x.status = 'suspect'
	UNION ALL
	SELECT x.id, 'rule', x.status, x.text, COALESCE(s.cause, ''), COALESCE(s.reason, ''), '', COALESCE(s.at, 0)
		FROM rules x ` + latestSuspicion + ` WHERE x.status = 'suspect'
	ORDER BY 8 DESC`

const atRiskQuery = `
	WITH RECURSIVE down(id, via, depth) AS (
		SELECT e.src, e.dst, 1 FROM edges e
			WHERE e.type IN ('used', 'derived_from')
			AND e.dst IN (SELECT id FROM decisions WHERE status = 'suspect' UNION SELECT id FROM memories WHERE status = 'suspect')
		UNION
		SELECT e.src, e.dst, down.depth + 1 FROM edges e JOIN down ON e.dst = down.id
			WHERE e.type IN ('used', 'derived_from') AND down.depth < 16
	)
	SELECT r.id, r.kind, r.status, r.summary, '', '', MIN(down.via), 0 FROM down JOIN (
		SELECT id, 'decision' AS kind, status, question || ' → ' || choice AS summary FROM decisions
		UNION ALL SELECT id, 'memory', status, content FROM memories
	) r ON r.id = down.id
	WHERE r.status = 'active'
	GROUP BY r.id ORDER BY r.id`

const conflictQuery = `
	SELECT e.dst, 'memory', 'forked', group_concat(m.id, ', '), '', 'superseded by more than one live version', '', 0
		FROM edges e JOIN memories m ON m.id = e.src
		WHERE e.type = 'supersedes' AND m.status IN ('active', 'suspect')
		GROUP BY e.dst HAVING COUNT(*) > 1
	UNION ALL
	SELECT e.dst, 'decision', 'forked', group_concat(d.id, ', '), '', 'superseded by more than one live decision', '', 0
		FROM edges e JOIN decisions d ON d.id = e.src
		WHERE e.type = 'supersedes' AND d.status IN ('active', 'suspect')
		GROUP BY e.dst HAVING COUNT(*) > 1`

// factCheckQuery finds active memories whose latest judgment asked for review.
const factCheckQuery = `
	SELECT m.id, 'memory', m.status, m.content, j.id, j.verdict || ' ' || printf('%.2f', j.confidence) || ' by ' || j.decided_by ||
		CASE WHEN j.reason != '' THEN ': ' || j.reason ELSE '' END, '', j.at
	FROM memories m JOIN judgments j ON j.rowid = (SELECT rowid FROM judgments WHERE subject = m.id ORDER BY at DESC LIMIT 1)
	WHERE m.status IN ('active', 'suspect') AND j.action = 'review'
	ORDER BY j.at DESC`

// Review lists everything that needs attention.
func (e *Engine) Review() (*Review, error) {
	rv := &Review{}
	var err error
	if rv.Suspect, err = selectAll(e.db, scanReviewItem, suspectQuery); err != nil {
		return nil, err
	}
	if rv.AtRisk, err = selectAll(e.db, scanReviewItem, atRiskQuery); err != nil {
		return nil, err
	}
	if rv.Conflicts, err = selectAll(e.db, scanReviewItem, conflictQuery); err != nil {
		return nil, err
	}
	if rv.FactCheck, err = selectAll(e.db, scanReviewItem, factCheckQuery); err != nil {
		return nil, err
	}
	keys, err := e.Keys()
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if !k.Trusted {
			rv.UntrustedKeys = append(rv.UntrustedKeys, k)
		}
	}
	health, err := e.JudgeHealth(24 * time.Hour)
	if err != nil {
		return nil, err
	}
	for _, h := range health {
		if h.Failed > 0 {
			rv.Judges = append(rv.Judges, h)
		}
	}
	return rv, nil
}
