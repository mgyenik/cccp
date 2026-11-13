package machine

import (
	"testing"
	"time"
)

var q0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestSumQueueNewestInputReplacesQueued(t *testing.T) {
	q := newSumQueue()
	q.offer(summaryJob{key: "a", hash: "h1", input: "old"}, q0)
	q.offer(summaryJob{key: "b", hash: "x", input: "b"}, q0)
	q.offer(summaryJob{key: "a", hash: "h2", input: "new"}, q0)
	if len(q.queue) != 2 || q.queue[0].hash != "h2" || q.queue[0].input != "new" {
		t.Fatalf("queue: %+v", q.queue)
	}
}

func TestSumQueueOneAtATimeAndNoDuplicateOfRunning(t *testing.T) {
	q := newSumQueue()
	q.offer(summaryJob{key: "a", hash: "h1"}, q0)
	q.offer(summaryJob{key: "b", hash: "h1"}, q0)
	j, ok := q.next(q0)
	if !ok || j.key != "a" {
		t.Fatal("first job")
	}
	if _, ok := q.next(q0); ok {
		t.Fatal("second job started while one runs")
	}
	if q.offer(summaryJob{key: "a", hash: "h1"}, q0) {
		t.Fatal("the running job was queued again")
	}
}

func TestSumQueueBackoffStopsRespawnLoop(t *testing.T) {
	q := newSumQueue()
	job := summaryJob{key: "a", hash: "h1"}
	q.offer(job, q0)
	q.next(q0)
	q.done("a", "h1", true, q0)
	// The daemon recomputes right after a result: the same input must not
	// be queued again during the backoff.
	if q.offer(job, q0.Add(time.Second)) {
		t.Fatal("failed job requeued immediately")
	}
	if !q.offer(job, q0.Add(backoffBase+time.Second)) {
		t.Fatal("not retried after the backoff")
	}
	q.next(q0.Add(backoffBase + time.Second))
	q.done("a", "h1", true, q0.Add(backoffBase+time.Second))
	if q.offer(job, q0.Add(2*backoffBase)) {
		t.Fatal("second backoff should be twice as long")
	}
	// A different input is not held back by the old failure.
	if !q.offer(summaryJob{key: "a", hash: "h2"}, q0.Add(2*backoffBase)) {
		t.Fatal("new input blocked by an old failure")
	}
}

func TestSumQueuePausesAfterConsecutiveFailures(t *testing.T) {
	q := newSumQueue()
	paused := false
	for i, k := range []string{"a", "b", "c"} {
		q.offer(summaryJob{key: k, hash: "h"}, q0)
		if _, ok := q.next(q0); !ok {
			t.Fatalf("job %d did not start", i)
		}
		paused = q.done(k, "h", true, q0)
	}
	if !paused {
		t.Fatal("three consecutive failures should pause")
	}
	if q.offer(summaryJob{key: "d", hash: "h"}, q0.Add(time.Minute)) {
		t.Fatal("offer accepted while paused")
	}
	if !q.offer(summaryJob{key: "d", hash: "h"}, q0.Add(pauseFor+time.Second)) {
		t.Fatal("still paused after pauseFor")
	}
}

func TestSumQueueSuccessResetsFailures(t *testing.T) {
	q := newSumQueue()
	q.offer(summaryJob{key: "a", hash: "h"}, q0)
	q.next(q0)
	q.done("a", "h", true, q0)
	q.offer(summaryJob{key: "b", hash: "h"}, q0)
	q.next(q0)
	q.done("b", "h", false, q0)
	if q.consecutive != 0 {
		t.Fatal("success should reset the consecutive count")
	}
}

// Summaries run before working titles (a notification can be waiting on a
// summary), and a title job belongs to its session.
func TestSummariesBeforeTitles(t *testing.T) {
	q := newSumQueue()
	now := time.Unix(0, 0)
	q.offer(summaryJob{key: titleKey("a"), hash: "t", title: true}, now)
	q.offer(summaryJob{key: "b", hash: "s"}, now)
	if j, _ := q.next(now); j.key != "b" {
		t.Fatalf("started %q first", j.key)
	}
	q.done("b", "s", false, now)
	if j, _ := q.next(now); j.key != titleKey("a") {
		t.Fatalf("then %q", j.key)
	}
	q.done(titleKey("a"), "t", true, now) // failed: backing off
	q.offer(summaryJob{key: titleKey("c"), hash: "x", title: true}, now)
	q.forget(map[string]bool{"a": true})
	if len(q.queue) != 0 || len(q.failures) != 1 {
		t.Fatalf("forget by session: queue %v failures %v", q.queue, q.failures)
	}
}
