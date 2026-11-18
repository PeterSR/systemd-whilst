package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func start(t *testing.T) (*Watcher, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan struct{}, 16)
	w, err := New(ctx, 50*time.Millisecond, func() { ch <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	return w, ch
}

func expect(t *testing.T, ch chan struct{}, want bool, what string) {
	t.Helper()
	select {
	case <-ch:
		if !want {
			t.Fatalf("%s: unexpected change", what)
		}
	case <-time.After(500 * time.Millisecond):
		if want {
			t.Fatalf("%s: no change seen", what)
		}
	}
}

func TestWatchWriteRenameAndCreate(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	w, ch := start(t)
	w.Set([]string{cfg}) // does not exist yet

	os.WriteFile(cfg, []byte("a"), 0o644)
	expect(t, ch, true, "create")

	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o644)
	expect(t, ch, false, "unrelated file")

	tmp := filepath.Join(dir, ".config.toml.swp")
	os.WriteFile(tmp, []byte("b"), 0o644)
	expect(t, ch, false, "temp file")
	os.Rename(tmp, cfg)
	expect(t, ch, true, "rename over")

	// A burst of writes gives one call.
	for i := 0; i < 5; i++ {
		os.WriteFile(cfg, []byte{byte(i)}, 0o644)
	}
	expect(t, ch, true, "burst")
	expect(t, ch, false, "burst coalesced")
}

func TestWatchSymlinkTarget(t *testing.T) {
	cfgDir, realDir := t.TempDir(), t.TempDir()
	target := filepath.Join(realDir, "whilst.toml")
	os.WriteFile(target, []byte("a"), 0o644)
	link := filepath.Join(cfgDir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	w, ch := start(t)
	w.Set([]string{link})
	os.WriteFile(target, []byte("b"), 0o644)
	expect(t, ch, true, "edit through symlink target")
}

func TestWatchSetDropsOldFiles(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a, b := filepath.Join(dirA, "a.toml"), filepath.Join(dirB, "b.toml")
	w, ch := start(t)
	w.Set([]string{a, b})
	w.Set([]string{a})
	os.WriteFile(b, []byte("x"), 0o644)
	expect(t, ch, false, "file no longer watched")
	os.WriteFile(a, []byte("x"), 0o644)
	expect(t, ch, true, "file still watched")
}

// Two paths to one directory share a watch; dropping one path keeps it.
func TestWatchSymlinkedDirShared(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	w, ch := start(t)
	w.Set([]string{filepath.Join(link, "a.toml"), filepath.Join(real, "a.toml")})
	w.Set([]string{filepath.Join(real, "a.toml")})
	os.WriteFile(filepath.Join(real, "a.toml"), []byte("x"), 0o644)
	expect(t, ch, true, "write after dropping the symlinked path")
}

// Set after the watcher stopped does nothing.
func TestWatchSetAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w, err := New(ctx, time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(1500 * time.Millisecond)
	w.Set([]string{filepath.Join(t.TempDir(), "a.toml")})
}
