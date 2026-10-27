package smem

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var words = []string{"auth", "deploy", "cache", "queue", "billing", "orders", "postgres", "redis", "staging", "prod",
	"runner", "build", "test", "race", "port", "config", "token", "schema", "migration", "index", "latency", "retry"}

func sentence(r *rand.Rand) string {
	s := ""
	for i := 0; i < 10; i++ {
		s += words[r.IntN(len(words))] + " "
	}
	return s + strconv.Itoa(r.IntN(1e6))
}

// TestScale measures the main paths at increasing store sizes. It is slow,
// so it only runs with SMEM_SCALE=1.
func TestScale(t *testing.T) {
	if os.Getenv("SMEM_SCALE") == "" {
		t.Skip("set SMEM_SCALE=1")
	}
	sizes := []int{1000, 10000, 50000}
	if s := os.Getenv("SMEM_SCALE_SIZES"); s != "" {
		sizes = nil
		for _, f := range splitComma(s) {
			n, _ := strconv.Atoi(f)
			sizes = append(sizes, n)
		}
	}
	for _, n := range sizes {
		r := rand.New(rand.NewPCG(1, uint64(n)))
		c := start()
		e := newEngine(t, "bench", c)
		t0 := time.Now()
		for i := 0; i < n; i++ {
			if _, err := e.Remember(MemoryInput{Content: sentence(r)}); err != nil {
				t.Fatal(err)
			}
			if i%100 == 0 {
				c.advance(time.Hour)
			}
		}
		write := time.Since(t0) / time.Duration(n)

		timeIt := func(reps int, fn func()) time.Duration {
			t0 := time.Now()
			for i := 0; i < reps; i++ {
				fn()
			}
			return time.Since(t0) / time.Duration(reps)
		}
		recall := timeIt(50, func() { e.Recall(RecallQuery{Text: "auth token " + words[r.IntN(len(words))], NoRecord: true}) })
		recallRec := timeIt(50, func() { e.Recall(RecallQuery{Text: "deploy staging", Limit: 10}) })
		ctx := timeIt(5, func() { e.Context(4000) })
		t0 = time.Now()
		rep, err := e.Verify()
		if err != nil || !rep.OK {
			t.Fatal(err, rep.Problems)
		}
		verify := time.Since(t0)

		var buf bytes.Buffer
		e.Export(&buf, "")
		evs, _ := ReadEvents(&buf)
		other := newEngine(t, "other", c)
		t0 = time.Now()
		if rep, err := other.Import(evs); err != nil || rep.Accepted != len(evs) {
			t.Fatal(err, rep.Rejected)
		}
		imp := time.Since(t0)
		st, _ := os.Stat(e.Path())
		wal, _ := os.Stat(e.Path() + "-wal")
		size := st.Size()
		if wal != nil {
			size += wal.Size()
		}
		fmt.Printf("n=%-6d write %-8v recall %-8v recall+record %-8v context %-8v verify %-8v (%.0f µs/event) import %-8v (%.0f events/s) db %.1f MB\n",
			n, write.Round(time.Microsecond), recall.Round(time.Microsecond), recallRec.Round(time.Microsecond),
			ctx.Round(time.Millisecond), verify.Round(time.Millisecond), float64(verify.Microseconds())/float64(rep.Events),
			imp.Round(time.Millisecond), float64(len(evs))/imp.Seconds(), float64(size)/1e6)
		_ = filepath.Base
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == ',' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(ch)
		}
	}
	return append(out, cur)
}
