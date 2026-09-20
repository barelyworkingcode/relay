package config

import (
	"fmt"
	"path/filepath"
	"syscall"
)

const vnodeChange = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_ATTRIB

// WatchSettingsFile calls notify, from one goroutine, whenever settings.json
// in dir may have changed. It is a kqueue watch on the directory (which sees
// an atomic rename replace the file) plus one on the file itself (which sees an
// in-place edit); the file watch is re-armed after every event because a
// replace leaves it pointing at the unlinked old file. It reads no settings:
// notify is only a prompt to submit an import.
//
// This is deliberate: there is no timer. A missed event is not recovered here;
// the next change or a restart converges.
func WatchSettingsFile(dir string, notify func()) (stop func(), err error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY, 0)
	if err != nil {
		syscall.Close(kq)
		return nil, fmt.Errorf("watch %s: %w", dir, err)
	}
	var wake [2]int
	if err := syscall.Pipe(wake[:]); err != nil {
		syscall.Close(dirFD)
		syscall.Close(kq)
		return nil, fmt.Errorf("wake pipe: %w", err)
	}
	register := func(fd, filter int, flags int, fflags uint32) error {
		var ev syscall.Kevent_t
		syscall.SetKevent(&ev, fd, filter, flags)
		ev.Fflags = fflags
		_, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil)
		return err
	}
	for _, r := range []struct {
		fd, filter, flags int
		fflags            uint32
	}{
		{dirFD, syscall.EVFILT_VNODE, syscall.EV_ADD | syscall.EV_CLEAR, vnodeChange},
		{wake[0], syscall.EVFILT_READ, syscall.EV_ADD, 0},
	} {
		if err := register(r.fd, r.filter, r.flags, r.fflags); err != nil {
			syscall.Close(wake[0])
			syscall.Close(wake[1])
			syscall.Close(dirFD)
			syscall.Close(kq)
			return nil, fmt.Errorf("register watch: %w", err)
		}
	}

	path := filepath.Join(dir, "settings.json")
	fileFD := -1
	arm := func() {
		if fileFD >= 0 {
			syscall.Close(fileFD) // closing drops its kevent
			fileFD = -1
		}
		fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
		if err != nil {
			return
		}
		if register(fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR, vnodeChange) != nil {
			syscall.Close(fd)
			return
		}
		fileFD = fd
	}
	arm()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if fileFD >= 0 {
				syscall.Close(fileFD)
			}
			syscall.Close(wake[0])
			syscall.Close(dirFD)
			syscall.Close(kq)
		}()
		events := make([]syscall.Kevent_t, 8)
		for {
			n, err := syscall.Kevent(kq, nil, events, nil)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				return
			}
			for _, ev := range events[:n] {
				if int(ev.Ident) == wake[0] {
					return
				}
			}
			arm()
			notify()
		}
	}()
	return func() {
		syscall.Write(wake[1], []byte{0})
		<-done
		syscall.Close(wake[1])
	}, nil
}
