//go:build linux

package transparentproxy

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type pinnedSocket struct {
	fd      int
	mark    uint32
	changed bool
}

func protectProcessSockets(cfg Config) (*socketProtection, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, fmt.Errorf("enumerate existing process sockets: %w", err)
	}
	var sockets []pinnedSocket
	finish := func(commit bool) error {
		var result error
		for _, socket := range sockets {
			if !commit && socket.changed {
				result = errors.Join(result, unix.SetsockoptInt(socket.fd, unix.SOL_SOCKET, unix.SO_MARK, int(socket.mark)))
			}
			result = errors.Join(result, unix.Close(socket.fd))
		}
		return result
	}
	fail := func(cause error) (*socketProtection, error) { return nil, errors.Join(cause, finish(false)) }
	var marks []uint32
	for _, entry := range entries {
		name := entry.Name()
		target, err := os.Readlink("/proc/self/fd/" + name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		if !strings.HasPrefix(target, "socket:[") {
			continue
		}
		fd, err := strconv.Atoi(name)
		if err != nil {
			return fail(err)
		}
		pinned, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("pin existing process socket: %w", err))
		}
		domain, err := unix.GetsockoptInt(pinned, unix.SOL_SOCKET, unix.SO_DOMAIN)
		if errors.Is(err, unix.ENOTSOCK) || err == nil && domain != unix.AF_INET && domain != unix.AF_INET6 {
			_ = unix.Close(pinned)
			continue
		}
		if err != nil {
			_ = unix.Close(pinned)
			return fail(err)
		}
		mark, err := unix.GetsockoptInt(pinned, unix.SOL_SOCKET, unix.SO_MARK)
		if err != nil {
			_ = unix.Close(pinned)
			return fail(fmt.Errorf("read existing socket mark: %w", err))
		}
		if err := validateOutboundMark(cfg, uint32(mark)); err != nil {
			_ = unix.Close(pinned)
			return fail(err)
		}
		sockets = append(sockets, pinnedSocket{fd: pinned, mark: uint32(mark)})
		if mark != 0 {
			marks = append(marks, uint32(mark))
		}
	}
	// Validate every pre-existing explicit mark before changing any socket.
	for i := range sockets {
		if sockets[i].mark != 0 {
			continue
		}
		if err := unix.SetsockoptInt(sockets[i].fd, unix.SOL_SOCKET, unix.SO_MARK, int(DefaultOutboundMark)); err != nil {
			return fail(fmt.Errorf("protect existing process socket before capture: %w", err))
		}
		sockets[i].changed = true
	}
	return &socketProtection{marks: uniqueMarks(marks), finish: finish}, nil
}
