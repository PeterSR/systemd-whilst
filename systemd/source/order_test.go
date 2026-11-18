package source

import (
	"context"
	"sync"
	"testing"
	"time"
)

// slowReader returns "old" slowly; a push of "new" lands during the read.
type slowReader struct {
	push    func(any)
	started chan struct{}
	release chan struct{}
	reads   int
}

func (r *slowReader) describe() string { return "slow" }
func (r *slowReader) watch(_ context.Context, push func(any), _ func()) error {
	r.push = push
	return nil
}
func (r *slowReader) read(context.Context) (any, error) {
	r.reads++
	if r.reads == 1 {
		close(r.started)
		<-r.release
		return "old", nil
	}
	return "new", nil
}

func TestReadOvertakenByPushIsRedone(t *testing.T) {
	r := &slowReader{started: make(chan struct{}), release: make(chan struct{})}
	tr := &triggered{fact: "x", spec: &Spec{}, r: r}
	var mu sync.Mutex
	var got []any
	done := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Start(ctx, func(u Update) {
		mu.Lock()
		got = append(got, u.Value)
		mu.Unlock()
		done <- struct{}{}
	}); err != nil {
		t.Fatal(err)
	}
	<-r.started
	r.push("new")
	close(r.release)
	<-done
	<-done
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "new" || got[1] != "new" {
		t.Fatalf("an older read overtook a push: %v", got)
	}
}
