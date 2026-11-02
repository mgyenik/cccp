package daemon

import (
	"errors"
	"os"
	"sync"
	"testing"
)

// sent records notifications the daemon would have shown. Every test in this
// package runs with it installed, so no test can reach the real desktop
// notifier.
var sent struct {
	mu   sync.Mutex
	argv [][]string
}

func testNotifier(argv []string) error {
	sent.mu.Lock()
	defer sent.mu.Unlock()
	sent.argv = append(sent.argv, argv)
	return nil
}

func sentNotifications() [][]string {
	sent.mu.Lock()
	defer sent.mu.Unlock()
	return append([][]string(nil), sent.argv...)
}

// refuseSummary stands in for the summarizer in every test: a test that
// wants summaries installs its own fake.
func refuseSummary(job summaryJob, model string) summaryResult {
	return summaryResult{key: job.key, err: errors.New("summaries are disabled in tests")}
}

func TestMain(m *testing.M) {
	notifyRunner = testNotifier
	summaryRunner = refuseSummary
	os.Exit(m.Run())
}

// The real adapters must refuse to act from a test binary, so a test that
// forgets to install a fake fails instead of touching the desktop or the
// user's Claude subscription.
func TestRealAdaptersRefuseInTests(t *testing.T) {
	if err := execNotifier([]string{"notify-send", "x", "y"}); err == nil {
		t.Fatal("execNotifier ran from a test")
	}
	if res := runSummary(summaryJob{key: "k"}, "haiku"); res.err == nil {
		t.Fatal("runSummary ran from a test")
	}
}
