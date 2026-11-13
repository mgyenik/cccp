package machine

import (
	"strings"
	"time"

	"cccp/internal/model"
)

// summaryJob asks for a one-line summary of one session's input.
type summaryJob struct {
	key     string // the queue key: the session's, or titleKey of it
	hash    string
	input   string
	profile model.Profile // the Claude profile to run under
	prompt  string        // the system prompt
	// title: a working title, made from the prompts up to basis.
	title bool
	basis time.Time
}

// titleKey is a session's working-title job's queue key: its own, beside
// the session's summary job.
func titleKey(session string) string { return "title:" + session }

// sessionOf is the session a queue key belongs to.
func sessionOf(key string) string { return strings.TrimPrefix(key, "title:") }

// Summary scheduling policy.
const (
	// A failed summary for the same input is retried after backoffBase,
	// doubling per consecutive failure, up to backoffMax.
	backoffBase = time.Minute
	backoffMax  = time.Hour
	// After pauseAfter consecutive failures across all sessions (logged
	// out, usage limit reached) every summary pauses for pauseFor.
	pauseAfter = 3
	pauseFor   = 15 * time.Minute
)

type failure struct {
	hash    string
	count   int
	retryAt time.Time
}

// sumQueue decides which summary jobs run. It holds at most one queued job
// per session (the newest input wins), runs one job at a time, and backs
// off on failures so a failing `claude -p` is never respawned in a loop.
// Retries are not timed: an expired backoff is acted on at the next
// recompute, which only happens when something changes.
type sumQueue struct {
	queue       []summaryJob
	running     string // key of the running job, "" when idle
	runningHash string
	failures    map[string]failure
	consecutive int
	pausedUntil time.Time
}

func newSumQueue() *sumQueue { return &sumQueue{failures: map[string]failure{}} }

// offer queues job unless it is already running, backing off, or paused.
// It reports whether the job was queued.
func (q *sumQueue) offer(job summaryJob, now time.Time) bool {
	if now.Before(q.pausedUntil) {
		return false
	}
	if q.running == job.key && q.runningHash == job.hash {
		return false
	}
	if f, ok := q.failures[job.key]; ok && f.hash == job.hash && now.Before(f.retryAt) {
		return false
	}
	for i, j := range q.queue {
		if j.key == job.key {
			q.queue[i] = job // newer input replaces the queued one
			return true
		}
	}
	q.queue = append(q.queue, job)
	return true
}

// next returns the job to start, if none is running and one is queued:
// summaries before titles (a notification can be waiting on a summary).
func (q *sumQueue) next(now time.Time) (summaryJob, bool) {
	if q.running != "" || len(q.queue) == 0 || now.Before(q.pausedUntil) {
		return summaryJob{}, false
	}
	i := 0
	for j, job := range q.queue {
		if !job.title {
			i = j
			break
		}
	}
	job := q.queue[i]
	q.queue = append(q.queue[:i:i], q.queue[i+1:]...)
	q.running, q.runningHash = job.key, job.hash
	return job, true
}

// done records a finished job. It returns true when this failure paused
// all summaries (so the caller can log it once).
func (q *sumQueue) done(key, hash string, failed bool, now time.Time) (paused bool) {
	q.running, q.runningHash = "", ""
	if !failed {
		delete(q.failures, key)
		q.consecutive = 0
		return false
	}
	f := q.failures[key]
	if f.hash != hash {
		f = failure{hash: hash}
	}
	f.count++
	d := backoffBase << (f.count - 1)
	if d > backoffMax || d <= 0 {
		d = backoffMax
	}
	f.retryAt = now.Add(d)
	q.failures[key] = f
	q.consecutive++
	if q.consecutive >= pauseAfter {
		q.consecutive = 0
		q.pausedUntil = now.Add(pauseFor)
		return true
	}
	return false
}

// stalled reports whether a summary of key's input hash will not come
// soon: it failed (a retry is a minute or more away) or all are paused.
func (q *sumQueue) stalled(key, hash string, now time.Time) bool {
	if now.Before(q.pausedUntil) {
		return true
	}
	f, ok := q.failures[key]
	return ok && f.hash == hash
}

// forget drops state for sessions that no longer exist.
func (q *sumQueue) forget(keep map[string]bool) {
	for k := range q.failures {
		if !keep[sessionOf(k)] {
			delete(q.failures, k)
		}
	}
	out := q.queue[:0]
	for _, j := range q.queue {
		if keep[sessionOf(j.key)] {
			out = append(out, j)
		}
	}
	q.queue = out
}
