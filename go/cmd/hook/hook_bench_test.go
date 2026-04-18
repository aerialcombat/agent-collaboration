package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// TestHotPathP99Regression is an in-process regression gate on the
// Open+ResolveSelf+ReadUnread+Close inner-loop shape. It is NOT the W1
// DoD surface — that's wall-clock fresh-exec at tests/hook-latency-exec.sh
// (budget <15ms). The in-process budget stays tight (10ms) because the
// inner loop has no Go-runtime init or driver-registration overhead to
// amortize; a regression that blows past 10ms here predicts one at exec
// scale.
//
// The benchmark bootstraps a fresh DB via scripts/peer-inbox-db.py (so
// the migrate binary seeds goose_db_version via the authoritative path),
// populates one unread row, and measures the end-to-end time for the Go
// store to (Open, ResolveSelf, ReadUnread, Close).
func TestHotPathP99Regression(t *testing.T) {
	if testing.Short() {
		t.Skip("skip benchmark in -short mode")
	}

	root := findRepoRoot(t)

	tmp := t.TempDir()
	db := filepath.Join(tmp, "bench.db")
	recvCWD := filepath.Join(tmp, "recv")
	sendCWD := filepath.Join(tmp, "send")
	if err := os.MkdirAll(recvCWD, 0o755); err != nil {
		t.Fatalf("mkdir recv: %v", err)
	}
	if err := os.MkdirAll(sendCWD, 0o755); err != nil {
		t.Fatalf("mkdir send: %v", err)
	}

	baseEnv := []string{"AGENT_COLLAB_INBOX_DB=" + db}
	recvEnv := append([]string{"AGENT_COLLAB_SESSION_KEY=benchkey"}, baseEnv...)
	sendEnv := append([]string{"AGENT_COLLAB_SESSION_KEY=sendkey"}, baseEnv...)

	runPy(t, root, recvEnv, []string{"session-register", "--cwd", recvCWD,
		"--label", "recv", "--agent", "claude", "--role", "peer"})
	runPy(t, root, sendEnv, []string{"session-register", "--cwd", sendCWD,
		"--label", "send", "--agent", "claude", "--role", "peer"})

	// Tell the Go store where the DB is for the bench iterations.
	t.Setenv("AGENT_COLLAB_INBOX_DB", db)
	t.Setenv("AGENT_COLLAB_SESSION_KEY", "benchkey")

	const iterations = 200

	// Pre-seed N unread rows in a single tx. Each iteration's ReadUnread
	// should return exactly one, then MarkRead moves on. This isolates
	// the measurement from Python spawn jitter + SQLite WAL checkpoint
	// storms from sustained background writes.
	seedRows(t, db, recvCWD, sendCWD, iterations)

	// Warm-up open — first sqlite.Open in a process pays one-time costs
	// (driver registration, pragmas). Real hook invocations pay this
	// every time (one-shot binary), but it's noisy in a back-to-back
	// bench because the Go runtime reuses the driver registration. Run
	// one warmup iteration that's NOT counted.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := sqlitestore.Open(ctx)
		if err != nil {
			cancel()
			t.Fatalf("warmup open: %v", err)
		}
		self, err := st.ResolveSelf(ctx, recvCWD, "benchkey")
		if err != nil {
			cancel()
			st.Close()
			t.Fatalf("warmup resolve: %v", err)
		}
		if _, err := st.ReadUnread(ctx, self); err != nil {
			cancel()
			st.Close()
			t.Fatalf("warmup read: %v", err)
		}
		st.Close()
		cancel()
	}

	durations := make([]time.Duration, 0, iterations)

	for i := 0; i < iterations; i++ {
		start := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := sqlitestore.Open(ctx)
		if err != nil {
			cancel()
			t.Fatalf("iter %d: open: %v", i, err)
		}
		self, err := st.ResolveSelf(ctx, recvCWD, "benchkey")
		if err != nil {
			cancel()
			st.Close()
			t.Fatalf("iter %d: resolve: %v", i, err)
		}
		if _, err := st.ReadUnread(ctx, self); err != nil {
			cancel()
			st.Close()
			t.Fatalf("iter %d: read: %v", i, err)
		}
		st.Close()
		cancel()

		durations = append(durations, time.Since(start))
	}

	sort := func(xs []time.Duration) {
		// insertion sort; n=200, no need for sort.Sort imports
		for i := 1; i < len(xs); i++ {
			for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
				xs[j-1], xs[j] = xs[j], xs[j-1]
			}
		}
	}
	sort(durations)

	p50 := durations[iterations/2]
	p95 := durations[iterations*95/100]
	p99 := durations[iterations*99/100]
	max := durations[iterations-1]

	t.Logf("hot path (Open+Resolve+ReadUnread+Close) over %d dirty-path iterations", iterations)
	t.Logf("  p50 = %s", p50)
	t.Logf("  p95 = %s", p95)
	t.Logf("  p99 = %s", p99)
	t.Logf("  max = %s", max)

	const budget = 10 * time.Millisecond
	if p99 > budget {
		t.Fatalf("inner-loop regression: p99 %s exceeds regression budget %s", p99, budget)
	}
}

// seedRows inserts n unread inbox rows directly via SQL, bypassing the
// Python send path. That keeps the bench focused on read-path latency
// rather than measuring Python's cross-process seeding jitter.
func seedRows(t *testing.T, dbPath, toCWD, fromCWD string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	stmt, err := tx.Prepare(`
		INSERT INTO inbox (to_cwd, to_label, from_cwd, from_label, body, created_at, room_key)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	defer stmt.Close()
	roomKey := fmt.Sprintf("cwd:%s#recv+send", toCWD)
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("bench iteration %d", i)
		if _, err := stmt.Exec(toCWD, "recv", fromCWD, "send", body, now, roomKey); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// runPy executes scripts/peer-inbox-db.py with the given subcommand args.
// Inherits env from the parent plus the provided additions.
func runPy(t *testing.T, root string, envAdd []string, args []string) {
	t.Helper()
	script := filepath.Join(root, "scripts", "peer-inbox-db.py")
	cmd := exec.Command("python3", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), envAdd...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python %v failed: %v\n%s", args, err, out)
	}
}

// findRepoRoot walks up from the test binary's directory looking for
// scripts/peer-inbox-db.py — the authoritative Python implementation.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cur := cwd
	for {
		candidate := filepath.Join(cur, "scripts", "peer-inbox-db.py")
		if _, err := os.Stat(candidate); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			t.Fatalf("cannot locate scripts/peer-inbox-db.py starting from %s", cwd)
		}
		cur = parent
	}
}
