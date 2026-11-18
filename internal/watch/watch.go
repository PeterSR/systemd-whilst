// Package watch calls a function when any of a set of files changes. It
// watches the files' directories, so files replaced by a rename (as many
// editors save) or created later are seen too.
package watch

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const mask = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
	unix.IN_CREATE | unix.IN_DELETE | unix.IN_ATTRIB

// Watcher watches a set of files.
type Watcher struct {
	fd       int
	delay    time.Duration
	onChange func()

	mu sync.Mutex
	// dirs maps each watched directory path to its watch descriptor. Two
	// paths to the same directory (a symlinked directory) share one.
	dirs map[string]int
	// names maps a watch descriptor to the file names wanted in it.
	names map[int]map[string]bool
	timer *time.Timer
}

// New starts a watcher. onChange runs on the watcher's own goroutine, delay
// after the last change in a burst. The watcher stops when ctx ends.
func New(ctx context.Context, delay time.Duration, onChange func()) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	w := &Watcher{fd: fd, delay: delay, onChange: onChange,
		dirs: map[string]int{}, names: map[int]map[string]bool{}}
	go w.loop(ctx)
	return w, nil
}

// Set replaces the watched files. Each file is watched under its own path
// and, if it is a symlink, under the path it points to.
func (w *Watcher) Set(files []string) {
	want := map[string]map[string]bool{}
	add := func(p string) {
		dir, name := filepath.Split(filepath.Clean(p))
		dir = filepath.Clean(dir)
		if want[dir] == nil {
			want[dir] = map[string]bool{}
		}
		want[dir][name] = true
	}
	for _, f := range files {
		if !filepath.IsAbs(f) {
			continue
		}
		add(f)
		if real, err := filepath.EvalSymlinks(f); err == nil && real != f {
			add(real)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fd < 0 {
		return // stopped
	}
	dirs := map[string]int{}
	names := map[int]map[string]bool{}
	for dir, files := range want {
		wd, ok := w.dirs[dir]
		if !ok {
			// Adding a watch for a directory already watched (under
			// another path, or still watched from before) returns its
			// existing descriptor.
			var err error
			if wd, err = unix.InotifyAddWatch(w.fd, dir, mask); err != nil {
				slog.Warn("cannot watch for config changes", "dir", dir, "error", err)
				continue
			}
		}
		dirs[dir] = wd
		if names[wd] == nil {
			names[wd] = map[string]bool{}
		}
		for n := range files {
			names[wd][n] = true
		}
	}
	for _, wd := range w.dirs {
		if names[wd] == nil {
			unix.InotifyRmWatch(w.fd, uint32(wd))
		}
	}
	w.dirs, w.names = dirs, names
}

func (w *Watcher) loop(ctx context.Context) {
	defer func() {
		w.mu.Lock()
		unix.Close(w.fd)
		w.fd = -1
		if w.timer != nil {
			w.timer.Stop()
		}
		w.mu.Unlock()
	}()
	buf := make([]byte, 64*1024)
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for ctx.Err() == nil {
		n, err := unix.Poll(fds, 1000)
		if err != nil && !errors.Is(err, unix.EINTR) {
			slog.Warn("config watcher stopped", "error", err)
			return
		}
		if n <= 0 {
			continue
		}
		size, err := unix.Read(w.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			slog.Warn("config watcher stopped", "error", err)
			return
		}
		if w.relevant(buf[:size]) {
			w.mu.Lock()
			if w.timer == nil {
				w.timer = time.AfterFunc(w.delay, func() {
					if ctx.Err() == nil {
						w.onChange()
					}
				})
			} else {
				w.timer.Reset(w.delay)
			}
			w.mu.Unlock()
		}
	}
}

// relevant reports whether any event in buf concerns a watched file.
func (w *Watcher) relevant(buf []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	hit := false
	for len(buf) >= unix.SizeofInotifyEvent {
		wd := int(int32(binary.NativeEndian.Uint32(buf[0:4])))
		m := binary.NativeEndian.Uint32(buf[4:8])
		nameLen := int(binary.NativeEndian.Uint32(buf[12:16]))
		end := unix.SizeofInotifyEvent + nameLen
		if end > len(buf) {
			break
		}
		name := string(trimNUL(buf[unix.SizeofInotifyEvent:end]))
		buf = buf[end:]
		if m&unix.IN_Q_OVERFLOW != 0 {
			hit = true // events were lost; reload to be safe
			continue
		}
		if m&unix.IN_IGNORED != 0 {
			// The directory went away. Set adds it again on the next
			// reload, which this triggers.
			for dir, d := range w.dirs {
				if d == wd {
					delete(w.dirs, dir)
				}
			}
			if w.names[wd] != nil {
				delete(w.names, wd)
				hit = true
			}
			continue
		}
		if w.names[wd][name] {
			hit = true
		}
	}
	return hit
}

func trimNUL(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// EnsureDir creates the directory of path if it is missing, so a config
// file written there later is seen.
func EnsureDir(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Debug("cannot create config directory", "error", err)
	}
}
