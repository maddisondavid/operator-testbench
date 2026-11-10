package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// BoundedWriter explicitly promises cancellation without abandoned writes.
// Implementations must not return until their write has stopped.
type BoundedWriter interface {
	WriteContext(context.Context, []byte) (int, error)
}
type fileSink struct {
	fd      int
	regular bool
}

func admit(w io.Writer) (BoundedWriter, func(), error) {
	if b, ok := w.(BoundedWriter); ok {
		return b, func() {}, nil
	}
	f, ok := w.(*os.File)
	if !ok {
		return nil, nil, errors.New("unsupported output")
	}
	raw, e := f.SyscallConn()
	if e != nil {
		return nil, nil, e
	}
	var sink *fileSink
	e = raw.Control(func(fd uintptr) {
		var st syscall.Stat_t
		if e = syscall.Fstat(int(fd), &st); e != nil {
			return
		}
		mode := st.Mode & syscall.S_IFMT
		regular := mode == syscall.S_IFREG
		if mode == syscall.S_IFCHR {
			var term syscall.Termios
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
			if errno != 0 {
				e = errors.New("unsupported output")
				return
			}
		} else if !regular && mode != syscall.S_IFIFO {
			e = errors.New("unsupported output")
			return
		}
		var owned int
		if regular {
			owned, e = syscall.Dup(int(fd))
			if e == nil {
				syscall.CloseOnExec(owned)
			}
		} else {
			// Reopen creates a distinct open-file description: dup would share O_NONBLOCK.
			owned, e = syscall.Open(fmt.Sprintf("/proc/self/fd/%d", fd), syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOCTTY, 0)
		}
		if e == nil {
			sink = &fileSink{owned, regular}
		}
	})
	if e != nil {
		return nil, nil, e
	}
	if sink == nil {
		return nil, nil, errors.New("unsupported output")
	}
	return sink, func() { syscall.Close(sink.fd) }, nil
}
func (s *fileSink) WriteContext(ctx context.Context, p []byte) (int, error) {
	for {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		n, e := syscall.Write(s.fd, p)
		if e == syscall.EINTR {
			continue
		}
		if e != syscall.EAGAIN {
			if n < 0 {
				n = 0
			}
			return n, e
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
}
