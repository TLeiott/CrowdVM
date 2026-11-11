package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two sandboxes boot from the same image. The first one's boot is cancelled
// (its sandbox is deleted) while the shared download runs. The second boot,
// whose own context is live, must still get the image.
func TestBughuntR5CancelledPullFailsJoinedPull(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img_r5", []byte("base image bytes"))
	hold := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.holdFull = hold
	h.srv.mu.Unlock()

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() { errA <- h.node.PullImage(ctxA, "bx_a", "img_r5") }()
	waitFor(t, "first download in flight", func() bool { return h.node.fetchesLen() > 0 })

	errB := make(chan error, 1)
	go func() { errB <- h.node.PullImage(context.Background(), "bx_b", "img_r5") }()
	time.Sleep(100 * time.Millisecond) // B joins A's fetch

	cancelA()
	<-errA
	close(hold)
	select {
	case err := <-errB:
		if err != nil {
			t.Fatalf("live pull failed because another caller's context was cancelled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("joined pull did not return")
	}
	if !h.drv.store.HasImage("img_r5") {
		t.Fatal("image not imported for the live caller")
	}
}

// A caller that joins an in-flight download must be able to give up when
// its own context ends (e.g. its sandbox is deleted).
func TestBughuntR5JoinedPullIgnoresOwnContext(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img_r5b", []byte("base image bytes"))
	hold := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.holdFull = hold
	h.srv.mu.Unlock()
	defer close(hold)

	go h.node.PullImage(context.Background(), "bx_a", "img_r5b")
	waitFor(t, "first download in flight", func() bool { return h.node.fetchesLen() > 0 })

	ctxB, cancelB := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelB()
	errB := make(chan error, 1)
	go func() { errB <- h.node.PullImage(ctxB, "bx_b", "img_r5b") }()
	select {
	case <-errB:
	case <-time.After(3 * time.Second):
		t.Fatal("joined pull still blocked 3s after its own context expired (200ms)")
	}
}

// A node killed mid-download leaves <data>/tmp/*.part behind. Nothing ever
// removes them, and dirSize counts them against the disk budget.
func TestBughuntR5StalePartFilesRemovedOnStart(t *testing.T) {
	var stale string
	h := start(t, func(c *Config) {
		dir := filepath.Join(c.DataDir, "tmp")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stale = filepath.Join(dir, "image-img1-123456.part")
		if err := os.WriteFile(stale, make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	_ = h
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(stale); err == nil {
		t.Fatalf("stale partial download %s survived a node start", stale)
	}
}

func (n *Node) fetchesLen() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.fetches)
}
