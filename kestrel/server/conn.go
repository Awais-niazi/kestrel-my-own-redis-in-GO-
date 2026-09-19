package server

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"kestrel/command"
	"kestrel/config"
	"kestrel/resp"
)

// connection couples a client's protocol state with its socket. The command
// layer only ever sees the command.Client half.
type connection struct {
	srv *Server
	nc  net.Conn
	cl  *command.Client
	rd  *resp.Reader
	wr  *resp.Writer

	// deadlineSet records whether a read deadline is currently armed, so the
	// loop can skip the syscall when no timeout is configured.
	deadlineSet bool

	// wmu guards wr once a delivery goroutine exists, because from that
	// point two goroutines write to this connection: this one with command
	// replies, and that one with pushes. It is taken only while pusher is
	// non-nil, so an ordinary connection never pays for it.
	wmu    sync.Mutex
	pusher chan struct{}
}

func (s *Server) acceptLoop(l net.Listener) {
	defer s.workers.Done()
	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
			}
			// A transient accept error (typically EMFILE) should back off
			// rather than spin, and must not take the listener down.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if isTemporary(err) {
				delay = nextBackoff(delay)
				s.log.Warn("accept failed, backing off", "err", err, "delay", delay)
				time.Sleep(delay)
				continue
			}
			s.log.Error("listener stopped", "err", err)
			return
		}
		delay = 0
		s.conns.Add(1)
		go s.serveConn(nc)
	}
}

func isTemporary(err error) bool {
	return strings.Contains(err.Error(), "too many open files") ||
		strings.Contains(err.Error(), "resource temporarily unavailable")
}

func nextBackoff(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return 5 * time.Millisecond
	case d >= time.Second:
		return time.Second
	default:
		return d * 2
	}
}

func (s *Server) serveConn(nc net.Conn) {
	defer s.conns.Done()
	defer nc.Close()

	snap := s.cfg.Snapshot()
	s.stats.TotalConnections.Add(1)

	if reason, ok := s.admitConnection(nc, snap); !ok {
		s.stats.RejectedConnections.Add(1)
		s.refuse(nc, reason)
		return
	}

	tuneSocket(nc, snap)

	c := &connection{srv: s, nc: nc}
	c.wr = resp.NewWriter(nc)
	c.wr.MaxBuffer = int(snap.ClientQueryBufferLimit)
	c.rd = resp.NewReader(nc, resp.Limits{
		MaxBulk:        int(snap.ProtoMaxBulkLen),
		MaxMultiBulk:   snap.ProtoMaxMultiBulkLen,
		MaxInline:      snap.ProtoMaxInlineLen,
		MaxQueryBuffer: int(snap.ClientQueryBufferLimit),
	})
	c.cl = command.NewClient(
		s.nextID.Add(1),
		nc.RemoteAddr().String(),
		nc.LocalAddr().String(),
		c.wr,
		s.ks.DB(0),
		snap.RequirePass == "",
	)

	s.registerConn(c)
	defer func() {
		// Subscriptions are dropped before the connection is forgotten, so
		// the registry can never hold a client whose socket has gone.
		c.stopPusher()
		s.pubsub.Remove(c.cl)
		s.watchers.Unwatch(c.cl)
		s.unregisterConn(c)
	}()

	s.log.Debug("client connected", "id", c.cl.ID, "addr", c.cl.Addr)
	c.loop()
	s.log.Debug("client disconnected", "id", c.cl.ID, "addr", c.cl.Addr)
}

// admitConnection applies the limits that must be checked before a client is
// given any resources.
func (s *Server) admitConnection(nc net.Conn, snap *config.Values) (string, bool) {
	if s.clientCount() >= snap.MaxClients {
		return "ERR max number of clients reached", false
	}
	// Protected mode: refuse anything that is not loopback while no password
	// is set, so an accidentally exposed instance is not an open door
	// (FR-7.5).
	if snap.ProtectedMode && snap.RequirePass == "" && !isLoopback(nc.RemoteAddr()) {
		return "DENIED Kestrel is running in protected mode because protected mode " +
			"is enabled and no password is set. Connections are only accepted from the " +
			"loopback interface. To fix this, set a password with 'requirepass', or " +
			"disable protected mode with 'protected-mode no' if the network is trusted.", false
	}
	return "", true
}

func (s *Server) refuse(nc net.Conn, reason string) {
	w := resp.NewWriter(nc)
	w.WriteValue(resp.Err(reason))
	w.Flush()
	s.log.Warn("connection refused", "addr", nc.RemoteAddr().String(), "reason", reason)
}

func isLoopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	if host == "" {
		return true // a unix socket has no host
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func tuneSocket(nc net.Conn, snap *config.Values) {
	tcp, ok := nc.(*net.TCPConn)
	if !ok {
		return
	}
	tcp.SetNoDelay(true)
	if snap.TCPKeepAlive > 0 {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(time.Duration(snap.TCPKeepAlive) * time.Second)
	}
}

func (s *Server) registerConn(c *connection) {
	s.clientsMu.Lock()
	s.clients[c.cl.ID] = c
	s.clientsMu.Unlock()
}

func (s *Server) unregisterConn(c *connection) {
	s.clientsMu.Lock()
	delete(s.clients, c.cl.ID)
	s.clientsMu.Unlock()
}

func (s *Server) clientCount() int {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	return len(s.clients)
}

// loop is the per-connection command loop.
//
// Pipelining falls out of it: the reader keeps handing over commands while
// its buffer holds any, and the writer only reaches the socket once the
// buffer is drained, so N pipelined commands cost one read and one write
// (§6.1, §11).
func (c *connection) loop() {
	s := c.srv
	for {
		select {
		case <-s.quit:
			return
		default:
		}

		// Setting a deadline touches the runtime poller, so it is only done
		// when the timeout is configured or when one is still in force from
		// a previous configuration.
		if timeout := s.cfg.Snapshot().Timeout; timeout > 0 {
			c.nc.SetReadDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
			c.deadlineSet = true
		} else if c.deadlineSet {
			c.nc.SetReadDeadline(time.Time{})
			c.deadlineSet = false
		}

		args, err := c.rd.ReadCommand()
		if err != nil {
			c.handleReadError(err)
			return
		}
		c.cl.Touch(time.Now())

		c.execute(args)

		// Keep draining the pipeline before paying for a write syscall.
		if !c.rd.Buffered() || c.cl.CloseAfterReply || c.cl.PSync != nil {
			if err := c.flush(); err != nil {
				return
			}
		}

		// A client that has just subscribed needs a goroutine to push to
		// it, because this one is about to block in a read. It is started
		// after the reply has been flushed, so the confirmation cannot be
		// overtaken by a message about the channel it confirms.
		if c.pusher == nil && c.cl.Subscribed() {
			c.startPusher()
		}
		if c.cl.Overflowed() {
			c.srv.log.Warn("disconnecting a subscriber that fell too far behind",
				"client", c.cl.Addr)
			return
		}
		if c.cl.CloseAfterReply {
			return
		}

		// PSYNC takes the connection out of the command loop for good. The
		// socket stops carrying commands and replies and starts carrying a
		// record stream, so this goroutine hands it over and does not come
		// back.
		if req := c.cl.PSync; req != nil {
			c.cl.PSync = nil
			s.serveReplica(c, req)
			return
		}
	}
}

func (c *connection) handleReadError(err error) {
	var perr *resp.ProtocolError
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		return
	case errors.As(err, &perr):
		// A malformed request gets an error and then the connection is
		// closed: after a framing error there is no way to know where the
		// next command starts (FR-1.7).
		c.wr.WriteValue(resp.Err("ERR " + perr.Error()))
		c.wr.Flush()
		c.srv.log.Debug("protocol error", "id", c.cl.ID, "addr", c.cl.Addr, "err", err)
	case errors.Is(err, resp.ErrQueryBufferLimit):
		c.wr.WriteValue(resp.Err("ERR Protocol error: unauthenticated multibulk length"))
		c.wr.Flush()
		c.srv.log.Warn("client exceeded the query buffer limit", "id", c.cl.ID, "addr", c.cl.Addr)
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			c.srv.log.Debug("client idle timeout", "id", c.cl.ID, "addr", c.cl.Addr)
			return
		}
		c.srv.log.Debug("read error", "id", c.cl.ID, "err", err)
	}
}

// idleReaper closes connections that have been silent for longer than the
// configured timeout. The read deadline already covers the common case; this
// exists so that a timeout lowered by CONFIG SET applies to connections that
// are already parked in a read.
func (s *Server) idleReaper() {
	defer s.workers.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case now := <-ticker.C:
			timeout := int64(s.cfg.Snapshot().Timeout)
			if timeout <= 0 {
				continue
			}
			s.clientsMu.Lock()
			for _, c := range s.clients {
				if c.cl.Replica {
					continue
				}
				if c.cl.IdleSeconds(now) > timeout {
					c.nc.SetReadDeadline(time.Now())
				}
			}
			s.clientsMu.Unlock()
		}
	}
}

// execute runs one command, holding the write lock when a delivery goroutine
// shares the connection.
func (c *connection) execute(args [][]byte) {
	if c.pusher == nil {
		command.Execute(c.srv, c.cl, args)
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	command.Execute(c.srv, c.cl, args)
}

func (c *connection) flush() error {
	if c.pusher == nil {
		return c.wr.Flush()
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.wr.Flush()
}

// startPusher begins delivering this client's subscriptions.
//
// Messages are written from their own goroutine because this connection's is
// blocked in a read for almost all of its life. The alternative -- having
// the publisher write here directly -- would make one slow subscriber a
// problem for every publisher in the server.
func (c *connection) startPusher() {
	stop := make(chan struct{})
	c.pusher = stop
	c.srv.workers.Add(1)
	go func() {
		defer c.srv.workers.Done()
		for {
			select {
			case <-stop:
				return
			case <-c.srv.quit:
				return
			case m := <-c.cl.Outbox():
				if err := c.push(m); err != nil {
					return
				}
			}
		}
	}()
}

// push writes one message and flushes it.
//
// Every message is flushed rather than batched. A subscriber is waiting for
// news, and holding a message back for a buffer that may not fill turns a
// notification system into a polling one.
func (c *connection) push(m command.Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.wr.WriteValue(command.MessageValue(m))
	return c.wr.Flush()
}

// stopPusher ends delivery when the connection closes.
func (c *connection) stopPusher() {
	if c.pusher != nil {
		close(c.pusher)
		c.pusher = nil
	}
}
