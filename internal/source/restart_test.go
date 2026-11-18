package source

// Tests for restarting a triggered source after a failed start and for
// goroutines left over from an earlier attempt.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/config"
)

// flakyReader: the first watch sees a signal (refresh) and then fails, the
// way a dbus fact with on_dbus would when a trigger's AddMatch fails after
// PropertiesChanged arrived.
type flakyReader struct {
	mu      sync.Mutex
	watches int
	reads   int
}

func (r *flakyReader) describe() string { return "flaky" }
func (r *flakyReader) watch(_ context.Context, _ func(any), refresh func()) error {
	r.mu.Lock()
	r.watches++
	n := r.watches
	r.mu.Unlock()
	if n == 1 {
		refresh()
		time.Sleep(10 * time.Millisecond) // read in flight
		return errors.New("add match: LimitsExceeded")
	}
	return nil
}
func (r *flakyReader) read(ctx context.Context) (any, error) {
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
	}
	return "v", nil
}

func TestRetryAfterFailedStartReads(t *testing.T) {
	r := &flakyReader{}
	tr := &triggered{fact: &config.Fact{Name: "x"}, r: r}
	parent, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	got := make(chan Update, 4)
	emit := func(u Update) { got <- u }
	// what engine.startSources does
	a1, c1 := context.WithCancel(parent)
	if err := tr.Start(a1, emit); err == nil {
		t.Fatal("expected failure")
	}
	c1()
	a2, c2 := context.WithCancel(parent)
	defer c2()
	if err := tr.Start(a2, emit); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-got:
		t.Logf("got %+v", u)
	case <-time.After(time.Second):
		tr.mu.Lock()
		t.Errorf("no read after successful retry; running=%v again=%v reads=%d", tr.running, tr.again, r.reads)
		tr.mu.Unlock()
	}
}

type ctlReader struct {
	mu    sync.Mutex
	reads map[context.Context]int
	block chan struct{}
}

func (r *ctlReader) describe() string                               { return "ctl" }
func (r *ctlReader) watch(context.Context, func(any), func()) error { return nil }
func (r *ctlReader) read(ctx context.Context) (any, error) {
	r.mu.Lock()
	r.reads[ctx]++
	b := r.block
	r.mu.Unlock()
	if b != nil {
		select {
		case <-b:
		case <-ctx.Done():
		}
	}
	return "v", nil
}

// A stale goroutine from an old attempt must not consume the new attempt's
// refresh request. The old goroutine has finished its read but is slow in
// emit; meanwhile a retry Start runs and a refresh is requested during the
// new read.
func TestStaleGoroutineDoesNotEatRefresh(t *testing.T) {
	r := &ctlReader{reads: map[context.Context]int{}}
	tr := &triggered{fact: &config.Fact{Name: "x"}, r: r}
	a1, c1 := context.WithCancel(context.Background())
	release := make(chan struct{})
	emitted1 := make(chan struct{})
	emit1 := func(Update) { close(emitted1); <-release }
	if err := tr.Start(a1, emit1); err != nil {
		t.Fatal(err)
	}
	<-emitted1 // old goroutine is inside emit, read done
	c1()
	block := make(chan struct{})
	r.mu.Lock()
	r.block = block
	r.mu.Unlock()
	a2, c2 := context.WithCancel(context.Background())
	defer c2()
	got := make(chan Update, 8)
	if err := tr.Start(a2, func(u Update) { got <- u }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // new read in flight
	tr.Refresh()                      // must cause one more read after
	close(release)                    // old goroutine resumes
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	r.block = nil
	r.mu.Unlock()
	close(block)
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	n := r.reads[a2]
	r.mu.Unlock()
	if n < 2 {
		t.Errorf("refresh requested during new read was lost: reads with new ctx = %d, emitted = %d", n, len(got))
	}
}
