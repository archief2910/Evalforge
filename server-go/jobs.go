package main

// Durable evaluation jobs.
//
// An experiment used to be a goroutine: a restart mid-run left it "running"
// until a timeout fired on a goroutine that no longer existed. A job row now
// owns the work, so the queue survives the process:
//
//	POST /evaluations  ->  experiment(running) + job(queued)   [one transaction]
//	dispatcher         ->  claim (status=running, lease=now+45s)
//	worker process     ->  heartbeat renews the lease every 15s
//	worker exit 0      ->  job done
//	worker crash/kill  ->  lease expires -> reaper requeues -> retried
//	attempts exhausted ->  job failed + experiment failed with a reason
//
// Everything is one SQL row update, so two dispatchers (or two processes
// sharing a database) cannot claim the same job: the UPDATE is the lock.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	jobQueued  = "queued"
	jobRunning = "running"
	jobDone    = "done"
	jobFailed  = "failed"
)

type job struct {
	ID           string
	ExperimentID string
	ProjectID    string
	Payload      string
	Attempts     int
	MaxAttempts  int
}

// jobLease is how long a claim is valid without a heartbeat. A crashed
// worker's job becomes claimable again after this window.
func jobLease() time.Duration {
	if v := os.Getenv("JOB_LEASE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 45 * time.Second
}

func jobHeartbeatInterval() time.Duration {
	l := jobLease()
	if l < 3 {
		return time.Second
	}
	return l / 3
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// enqueueEvalJobTx writes the job in the same transaction as its experiment,
// so a crash cannot leave an experiment with no owner.
func enqueueEvalJobTx(tx *sql.Tx, expID, projectID string, in evalIn) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ts := nowUTC()
	_, err = tx.Exec(`INSERT INTO jobs(id, kind, experiment_id, project_id, payload, status,
		attempts, max_attempts, created_at, updated_at)
		VALUES (?,?,?,?,?,?,0,2,?,?)`,
		newID(), "eval", expID, projectID, string(payload), jobQueued, ts, ts)
	return err
}

// claimJob picks the oldest queued job (or one whose lease expired) and marks
// it running. The WHERE clause is the mutual exclusion: the second caller's
// update matches 0 rows and it moves on.
func claimJob(now time.Time) (*job, error) {
	ts := now.UTC().Format(time.RFC3339Nano)
	lease := now.Add(jobLease()).UTC().Format(time.RFC3339Nano)

	dbMu.Lock()
	defer dbMu.Unlock()

	var id string
	err := db.QueryRow(`SELECT id FROM jobs
		WHERE status=? OR (status=? AND (lease_expires_at IS NULL OR lease_expires_at < ?))
		ORDER BY created_at LIMIT 1`, jobQueued, jobRunning, ts).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	res, err := db.Exec(`UPDATE jobs SET status=?, attempts=attempts+1, lease_expires_at=?, updated_at=?
		WHERE id=? AND (status=? OR lease_expires_at < ?)`,
		jobRunning, lease, ts, id, jobQueued, ts)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // lost the race; try again next tick
	}

	var j job
	err = db.QueryRow(`SELECT id, experiment_id, project_id, payload, attempts, max_attempts
		FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.ExperimentID, &j.ProjectID, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func renewLease(jobID string) {
	until := time.Now().Add(jobLease()).UTC().Format(time.RFC3339Nano)
	dbMu.Lock()
	db.Exec(`UPDATE jobs SET lease_expires_at=?, updated_at=? WHERE id=?`, until, nowUTC(), jobID)
	dbMu.Unlock()
}

func markJob(jobID, status, errMsg string) {
	dbMu.Lock()
	db.Exec(`UPDATE jobs SET status=?, last_error=?, lease_expires_at=NULL, updated_at=? WHERE id=?`,
		status, nilIfEmpty(errMsg), nowUTC(), jobID)
	dbMu.Unlock()
}

// retireExperiment fails an experiment that no worker will ever finish, so the
// UI never shows "running" for something that is not running.
func retireExperiment(expID, reason string) {
	dbMu.Lock()
	db.Exec(`UPDATE experiments SET status='failed', summary=? WHERE id=? AND status='running'`,
		j(map[string]string{"error": reason}), expID)
	dbMu.Unlock()
}

// requeueOrphanedJobs runs at boot: this process is the only writer, so any
// job still marked running was owned by a dead process. Requeue it instead of
// waiting for the lease to expire.
func requeueOrphanedJobs() int {
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE jobs SET status=?, lease_expires_at=NULL, updated_at=? WHERE status=?`,
		jobQueued, nowUTC(), jobRunning)
	if err != nil {
		log.Printf("requeue orphaned jobs: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// reapExpiredLeases requeues jobs whose worker died without reporting. It runs
// on a ticker, not only at boot, so a panic in the dispatcher cannot wedge the
// queue permanently.
func reapExpiredLeases() int {
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE jobs SET status=?, lease_expires_at=NULL, updated_at=?
		WHERE status=? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		jobQueued, nowUTC(), jobRunning, nowUTC())
	if err != nil {
		log.Printf("reap expired leases: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// jobCounts feeds /health so queue depth is observable without a metrics stack.
func jobCounts() map[string]int {
	out := map[string]int{jobQueued: 0, jobRunning: 0, jobDone: 0, jobFailed: 0}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// runEvalJob executes one claimed job and decides its fate. It keeps the old
// worker semantics (bounded subprocess, environment, callback) and adds:
//   - a heartbeat, so a long eval is not reaped mid-flight
//   - a retry on abnormal exit (not on timeout: a timed-out eval is too
//     expensive to repeat automatically)
//   - an experiment status check, so "job done but experiment left running"
//     cannot happen silently
func runEvalJob(ctx context.Context, j *job) {
	var in evalIn
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
		markJob(j.ID, jobFailed, "bad job payload: "+err.Error())
		retireExperiment(j.ExperimentID, "bad job payload")
		return
	}

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		t := time.NewTicker(jobHeartbeatInterval())
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				renewLease(j.ID)
			}
		}
	}()

	errText, timedOut := runWorkerJob(ctx, j.ExperimentID, in, j.ProjectID)
	switch {
	case errText == "" && !timedOut:
		markJob(j.ID, jobDone, "")
		// The worker reports results over HTTP before exiting. If it exited
		// cleanly without a callback, the experiment would otherwise sit at
		// "running" forever — fail it with a diagnosable reason.
		var status string
		db.QueryRow(`SELECT status FROM experiments WHERE id=?`, j.ExperimentID).Scan(&status)
		if status == "running" {
			retireExperiment(j.ExperimentID, "worker exited without reporting results")
		}
		log.Printf("job %s done (experiment %s)", j.ID, j.ExperimentID)

	case timedOut || ctx.Err() != nil:
		// Shutdown or deadline: don't burn another full timeout.
		if ctx.Err() == context.Canceled {
			markJob(j.ID, jobQueued, "released on shutdown")
			log.Printf("job %s released for restart", j.ID)
			return
		}
		markJob(j.ID, jobFailed, errText)
		retireExperiment(j.ExperimentID, errText)

	case j.Attempts < j.MaxAttempts:
		markJob(j.ID, jobQueued, errText)
		log.Printf("job %s retry %d/%d: %s", j.ID, j.Attempts, j.MaxAttempts, trimForLog(errText))

	default:
		markJob(j.ID, jobFailed, errText)
		retireExperiment(j.ExperimentID, errText)
		log.Printf("job %s failed after %d attempts: %s", j.ID, j.Attempts, trimForLog(errText))
	}
}

// startDispatcher owns the worker loop. One claim at a time keeps evaluation
// load predictable (the worker itself parallelises records); raise this with a
// semaphore if the provider quota allows it.
func startDispatcher(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		lastReap := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if time.Since(lastReap) > 10*time.Second {
				if n := reapExpiredLeases(); n > 0 {
					log.Printf("reaper requeued %d expired job(s)", n)
				}
				lastReap = time.Now()
			}
			j, err := claimJob(time.Now())
			if err != nil {
				log.Printf("claim job: %v", err)
				continue
			}
			if j == nil {
				continue
			}
			runEvalJob(ctx, j)
		}
	}()
	return &wg
}

func trimForLog(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}
