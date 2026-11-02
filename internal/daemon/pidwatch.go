package daemon

import (
	"sync"

	"golang.org/x/sys/unix"
)

// pidWatcher reports process exits via pidfds on one epoll instance: a
// single goroutine blocked in epoll_wait, no polling.
type pidWatcher struct {
	epfd int
	mu   sync.Mutex
	fds  map[int]int // fd → pid
	pids map[int]int // pid → fd
	out  chan<- int
}

func newPidWatcher(out chan<- int) (*pidWatcher, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	w := &pidWatcher{epfd: epfd, fds: map[int]int{}, pids: map[int]int{}, out: out}
	go w.loop()
	return w, nil
}

// Watch starts watching pid; repeated calls are no-ops.
func (w *pidWatcher) Watch(pid int) {
	if pid <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pids[pid]; ok {
		return
	}
	fd, err := unix.PidfdOpen(pid, unix.PIDFD_NONBLOCK)
	if err != nil {
		return // already gone; the next snapshot will see it
	}
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)}
	if err := unix.EpollCtl(w.epfd, unix.EPOLL_CTL_ADD, fd, &ev); err != nil {
		unix.Close(fd)
		return
	}
	w.fds[fd], w.pids[pid] = pid, fd
}

func (w *pidWatcher) loop() {
	evs := make([]unix.EpollEvent, 16)
	for {
		n, err := unix.EpollWait(w.epfd, evs, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			fd := int(evs[i].Fd)
			w.mu.Lock()
			pid, ok := w.fds[fd]
			if ok {
				_ = unix.EpollCtl(w.epfd, unix.EPOLL_CTL_DEL, fd, nil)
				unix.Close(fd)
				delete(w.fds, fd)
				delete(w.pids, pid)
			}
			w.mu.Unlock()
			if ok {
				w.out <- pid
			}
		}
	}
}
