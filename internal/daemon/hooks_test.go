package daemon

import (
	"os"
	"testing"

	"github.com/justinrush/q/internal/api"
)

// close must not return while run is still applying an event: test cleanup
// removes q's directories right after, and an orphan still being recorded
// makes that removal fail.
func TestHookQueueCloseWaitsForQueuedEvents(t *testing.T) {
	svc := newTestService(t)
	queue := newHookQueue()
	go queue.run(svc)

	if !queue.offer(api.HookRequest{Tool: "cursor", Event: "stop"}) {
		t.Fatal("queue refused an event")
	}
	queue.close()

	if _, err := os.Stat(svc.dirs.OrphansFile()); err != nil {
		t.Fatalf("queued event not applied before close returned: %v", err)
	}
}
