package proxy

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Queue membership, pairing and removal are owned by server.mu. The watcher
// never reads application bytes and may close a connection only while its exact
// entry is still queued. Once paired, the forwarding goroutine owns it instead.
func (server *server) queuePendingLocked(registry *map[string][]*pendingConnection, key string, connection net.Conn, count *atomic.Int64) {
	pending := &pendingConnection{connection: connection, watchDone: make(chan struct{})}
	(*registry)[key] = append((*registry)[key], pending)
	count.Add(1)
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		defer close(pending.watchDone)
		err := waitPendingHangup(connection)
		server.mu.Lock()
		queue := (*registry)[key]
		for i, entry := range queue {
			if entry != pending {
				continue
			}
			copy(queue[i:], queue[i+1:])
			queue[len(queue)-1] = nil
			queue = queue[:len(queue)-1]
			if len(queue) == 0 {
				delete(*registry, key)
			} else {
				(*registry)[key] = queue
			}
			count.Add(-1)
			server.untrackLocked(connection)
			if err != nil && !server.closed {
				log.Printf("pending connection failed endpoint=%s error=%v", key, err)
			}
			server.mu.Unlock()
			_ = connection.Close()
			return
		}
		// Pairing or reconfiguration already removed this entry. In particular,
		// the deadline used to stop the watcher must not close a live pair.
		server.mu.Unlock()
	}()
}

func (server *server) popLivePendingLocked(registry *map[string][]*pendingConnection, key string, count *atomic.Int64) *pendingConnection {
	for {
		pending := popPending(registry, key)
		if pending == nil {
			return nil
		}
		count.Add(-1)
		// The watcher may not have processed the hangup yet. Do not give its
		// dead peer to a new client while waiting for that goroutine to run.
		closed, err := pendingHangup(pending.connection)
		if err != nil || closed {
			server.untrackLocked(pending.connection)
			_ = pending.connection.Close()
			continue
		}
		// No forwarding reads have started. Wake RawConn.Read without closing
		// the socket; runPair joins the watcher and clears this deadline.
		_ = pending.connection.SetReadDeadline(time.Now())
		return pending
	}
}

func pendingRawConn(connection net.Conn) (syscall.RawConn, error) {
	socket, ok := connection.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("pending connection does not expose a socket")
	}
	return socket.SyscallConn()
}

func pendingHangup(connection net.Conn) (closed bool, err error) {
	raw, err := pendingRawConn(connection)
	if err != nil {
		return false, err
	}
	var pollErr error
	err = raw.Control(func(fd uintptr) { closed, pollErr = socketHangup(fd) })
	return closed, errors.Join(err, pollErr)
}

func waitPendingHangup(connection net.Conn) error {
	raw, err := pendingRawConn(connection)
	if err != nil {
		return err
	}
	var pollErr error
	err = raw.Read(func(fd uintptr) bool {
		var closed bool
		closed, pollErr = socketHangup(fd)
		return closed || pollErr != nil
	})
	return errors.Join(err, pollErr)
}

func socketHangup(fd uintptr) (bool, error) {
	// POLLHUP/POLLERR are reported even with no requested events. Readability
	// alone is not disconnection: it may be queued data or a write-half-close.
	// RawConn.Read supplies the blocking wait through Go's socket poller.
	fds := []unix.PollFd{{Fd: int32(fd)}}
	for {
		_, err := unix.Poll(fds, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0, err
	}
}
