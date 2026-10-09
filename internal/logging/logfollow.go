package logging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const vnodeChange = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_ATTRIB

// WaitResult says why Wait returned.
type WaitResult int

const (
	WaitChanged     WaitResult = iota // a watched file or the directory changed
	WaitTimeout                       // the deadline passed
	WaitInterrupted                   // Wake was called
)

// tail is one open log file and how far it has been read. It is keyed by the
// file, not the path: after a rotation the descriptor still names the file
// its writer holds, which is the only way to see a writer stranded in ".1".
type tail struct {
	f       *os.File
	dev     uint64
	ino     uint64
	off     int64
	partial []byte
	lastTS  time.Time
}

// Tailer reads relay's log files under one logs directory and follows them.
// It opens nothing it did not find and creates nothing.
type Tailer struct {
	dir   string
	tails []*tail

	kq   int // -1 until Watch
	wake [2]int
	dirF *os.File
}

// OpenTailer opens the log files that exist in dir. A file that exists but
// cannot be opened is an error.
func OpenTailer(dir string) (*Tailer, error) {
	t := &Tailer{dir: dir, kq: -1}
	if err := t.discover(); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// HasRelayLog reports whether either generation of relay's own log is open.
func (t *Tailer) HasRelayLog() bool {
	for _, tl := range t.tails {
		if tl.f.Name() == filepath.Join(t.dir, "relay.log") || tl.f.Name() == filepath.Join(t.dir, "relay.log.1") {
			return true
		}
	}
	return false
}

// discover opens every log file whose inode is not already open.
func (t *Tailer) discover() error {
	for _, name := range LogFileNames() {
		path := filepath.Join(t.dir, name)
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", path, err)
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
			_ = f.Close()
			return fmt.Errorf("cannot read %s: %w", path, err)
		}
		if t.has(uint64(st.Dev), st.Ino) {
			_ = f.Close()
			continue
		}
		tl := &tail{f: f, dev: uint64(st.Dev), ino: st.Ino}
		t.tails = append(t.tails, tl)
		if t.kq >= 0 {
			if err := t.register(int(f.Fd())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tailer) has(dev, ino uint64) bool {
	for _, tl := range t.tails {
		if tl.dev == dev && tl.ino == ino {
			return true
		}
	}
	return false
}

// Drain returns the complete lines appended since the last call, in file
// order, and opens any file that appeared or replaced a rotated one. A file
// that was deleted is read to its end and then closed.
func (t *Tailer) Drain() ([]Line, error) {
	if err := t.discover(); err != nil {
		return nil, err
	}
	var out []Line
	kept := t.tails[:0]
	for _, tl := range t.tails {
		lines, err := tl.read()
		if err != nil {
			return out, fmt.Errorf("cannot read %s: %w", tl.f.Name(), err)
		}
		out = append(out, lines...)
		var st syscall.Stat_t
		if syscall.Fstat(int(tl.f.Fd()), &st) == nil && st.Nlink == 0 {
			_ = tl.f.Close()
			continue
		}
		kept = append(kept, tl)
	}
	t.tails = kept
	return out, nil
}

func (tl *tail) read() ([]Line, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(tl.f.Fd()), &st); err == nil && st.Size < tl.off {
		tl.off, tl.partial = 0, nil // truncated in place
	}
	var lines []Line
	buf := make([]byte, 64*1024)
	for {
		n, err := tl.f.ReadAt(buf, tl.off)
		tl.off += int64(n)
		tl.partial = append(tl.partial, buf[:n]...)
		for {
			i := bytes.IndexByte(tl.partial, '\n')
			if i < 0 {
				break
			}
			raw := append([]byte(nil), tl.partial[:i]...)
			tl.partial = tl.partial[i+1:]
			if len(raw) == 0 {
				continue
			}
			l := ParseLine(raw)
			// A line with no timestamp sorts beside the line before it.
			if l.TS.IsZero() {
				l.TS = tl.lastTS
			} else {
				tl.lastTS = l.TS
			}
			lines = append(lines, l)
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
	}
}

// SortLines orders lines by timestamp. It is stable, so equal timestamps keep
// the file order Drain returned.
func SortLines(lines []Line) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].TS.Before(lines[j].TS) })
}

// Watch arms kqueue on the logs directory and every open file. There is no
// timer: Wait blocks on vnode events and the caller's deadline.
func (t *Tailer) Watch() error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	t.kq = kq
	if err := syscall.Pipe(t.wake[:]); err != nil {
		return fmt.Errorf("wake pipe: %w", err)
	}
	if t.dirF, err = os.Open(t.dir); err != nil {
		return fmt.Errorf("watch %s: %w", t.dir, err)
	}
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, t.wake[0], syscall.EVFILT_READ, syscall.EV_ADD)
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		return fmt.Errorf("register wake: %w", err)
	}
	if err := t.register(int(t.dirF.Fd())); err != nil {
		return err
	}
	for _, tl := range t.tails {
		if err := t.register(int(tl.f.Fd())); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tailer) register(fd int) error {
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	ev.Fflags = vnodeChange
	if _, err := syscall.Kevent(t.kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		return fmt.Errorf("register watch: %w", err)
	}
	return nil
}

// Wake makes a blocked or later Wait return WaitInterrupted. It is safe to
// call from another goroutine once Watch has returned.
func (t *Tailer) Wake() {
	_, _ = syscall.Write(t.wake[1], []byte{0})
}

// Wait blocks until a watched file or the directory changes, Wake is called,
// or deadline passes. A zero deadline waits without limit.
func (t *Tailer) Wait(deadline time.Time) WaitResult {
	events := make([]syscall.Kevent_t, 8)
	for {
		var timeout *syscall.Timespec
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return WaitTimeout
			}
			ts := syscall.NsecToTimespec(remaining.Nanoseconds())
			timeout = &ts
		}
		n, err := syscall.Kevent(t.kq, nil, events, timeout)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return WaitInterrupted
		}
		if n == 0 {
			return WaitTimeout
		}
		for _, ev := range events[:n] {
			if int(ev.Ident) == t.wake[0] {
				return WaitInterrupted
			}
		}
		return WaitChanged
	}
}

// Close releases every descriptor.
func (t *Tailer) Close() {
	for _, tl := range t.tails {
		_ = tl.f.Close()
	}
	t.tails = nil
	if t.dirF != nil {
		_ = t.dirF.Close()
	}
	if t.kq >= 0 {
		_ = syscall.Close(t.kq)
		_ = syscall.Close(t.wake[0])
		_ = syscall.Close(t.wake[1])
		t.kq = -1
	}
}
