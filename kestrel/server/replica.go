package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kestrel/command"
	"kestrel/persist"
	"kestrel/resp"
)

// Replication, replica side.
//
// A replica runs one goroutine that connects to its leader, synchronises,
// and then applies the record stream until the link breaks, at which point
// it reconnects and tries to resume. Nothing else in the server drives it:
// REPLICAOF starts and stops it, and the rest is that goroutine's business.
//
// A replica in this build applies to its keyspace and does not write its own
// log, so a restart resynchronises from scratch. Persisting a replica's log
// needs an invariant the leader does not: a shipped snapshot is only usable
// once the stream has reached the end of the window its anchors span, since
// before that the shards are at instants the recorded offset does not
// describe. That is built deliberately rather than bolted on here.

// replicaState is what a replica knows about its leader.
type replicaState struct {
	mu     sync.Mutex
	host   string
	port   int
	cancel context.CancelFunc
	done   chan struct{}

	linkUp   atomic.Bool
	syncing  atomic.Bool
	offset   atomic.Uint64
	lastIO   atomic.Int64 // unix seconds
	lastErr  atomic.Value // string
	fullSync atomic.Int64

	replID string // guarded by mu
}

func (r *replicaState) status() string {
	switch {
	case r.linkUp.Load():
		return "up"
	case r.syncing.Load():
		return "sync"
	default:
		return "down"
	}
}

// Follow makes this server a replica of host:port, or promotes it to a leader
// when host is empty. It satisfies the command layer's Host.
func (s *Server) Follow(host string, port int) error {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()

	// Whatever happens, the previous link stops first. Two replication
	// goroutines applying two streams into one keyspace would interleave
	// two histories.
	if old := s.replica; old != nil {
		old.cancel()
		<-old.done
		s.replica = nil
	}

	if host == "" {
		if s.ks.IsReplica() {
			s.log.Info("promoted to leader")
		}
		s.ks.SetReplica(false)
		s.isReplica.Store(false)
		return nil
	}
	if s.persist == nil {
		return errors.New("replication requires appendonly yes")
	}

	ctx, cancel := context.WithCancel(context.Background())
	st := &replicaState{host: host, port: port, cancel: cancel, done: make(chan struct{})}
	s.replica = st
	s.isReplica.Store(true)
	// A replica must not expire on its own clock: it waits for the leader's
	// DEL (FR-3.4).
	s.ks.SetReplica(true)
	s.log.Info("replicating from", "host", host, "port", port)

	go s.replicaLoop(ctx, st)
	return nil
}

// replicaLoop keeps a link to the leader up, reconnecting with backoff.
func (s *Server) replicaLoop(ctx context.Context, st *replicaState) {
	defer close(st.done)
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		default:
		}

		err := s.replicaSession(ctx, st)
		st.linkUp.Store(false)
		st.syncing.Store(false)
		if err != nil && ctx.Err() == nil {
			st.lastErr.Store(err.Error())
			s.log.Warn("replication link lost", "leader",
				net.JoinHostPort(st.host, strconv.Itoa(st.port)),
				"error", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// replicaSession runs one connection to the leader, from dial to break.
func (s *Server) replicaSession(ctx context.Context, st *replicaState) error {
	addr := net.JoinHostPort(st.host, strconv.Itoa(st.port))
	d := net.Dialer{Timeout: 5 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer nc.Close()
	// The dial and the handshake are bounded; the stream that follows is
	// not, because a quiet leader legitimately sends nothing for a long
	// time.
	go func() {
		<-ctx.Done()
		nc.Close()
	}()

	br := bufio.NewReaderSize(nc, 64<<10)
	st.syncing.Store(true)

	listening := s.cfg.Snapshot().Port
	if err := writeCommand(nc, "REPLCONF", "listening-port", strconv.Itoa(listening)); err != nil {
		return err
	}
	if line, err := readLine(br); err != nil {
		return err
	} else if !strings.HasPrefix(line, "+") {
		return fmt.Errorf("REPLCONF refused: %s", line)
	}

	// An offset is only offered when this process has already applied part
	// of this leader's stream. Without persistence there is nothing to
	// resume from across a restart, so the first attempt is always full.
	id, off := "?", "0"
	if prev, ok := st.lastReplID(); ok {
		id, off = prev, strconv.FormatUint(st.offset.Load(), 10)
	}
	if err := writeCommand(nc, "PSYNC", id, off); err != nil {
		return err
	}
	head, err := readLine(br)
	if err != nil {
		return err
	}

	var from uint64
	var anchors persist.Anchors
	switch {
	case strings.HasPrefix(head, "+CONTINUE"):
		from = st.offset.Load()
		s.log.Info("resumed replication", "offset", from)
	case strings.HasPrefix(head, "+FULLRESYNC"):
		from, anchors, err = s.fullResync(st, head, br)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("the leader refused to synchronise: %s", head)
	}
	st.offset.Store(from)
	st.syncing.Store(false)
	st.linkUp.Store(true)
	st.lastIO.Store(time.Now().Unix())

	return s.applyStream(ctx, st, nc, br, from, anchors)
}

// fullResync receives a snapshot and loads it, replacing everything.
func (s *Server) fullResync(st *replicaState, head string, br *bufio.Reader) (uint64, persist.Anchors, error) {
	f := strings.Fields(head)
	if len(f) != 3 {
		return 0, nil, fmt.Errorf("malformed FULLRESYNC: %q", head)
	}
	st.setReplID(f[1])
	first, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("FULLRESYNC offset %q: %w", f[2], err)
	}

	body, err := readBulkPayload(br)
	if err != nil {
		return 0, nil, err
	}
	// The snapshot is written to a file before it is loaded, rather than
	// parsed from memory, so that the loader is the same code a restart
	// runs. A second implementation of it would be a second place for the
	// two to disagree.
	tmp := filepath.Join(s.persist.dir, "kestrel.incoming-snapshot")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return 0, nil, err
	}
	defer os.Remove(tmp)

	s.ks.SetLoading(true)
	defer s.ks.SetLoading(false)
	// Everything held locally belongs to a history this replica is being
	// told to abandon.
	s.ks.FlushAll()

	load, err := persist.LoadSnapshot(tmp, command.NewReplayer(s))
	if err != nil {
		return 0, nil, fmt.Errorf("loading the leader's snapshot: %w", err)
	}
	st.fullSync.Add(1)
	s.log.Info("full resynchronisation complete", "records", load.Records,
		"shards", len(load.Anchors), "from_offset", load.First,
		"snapshot_bytes", len(body))
	if load.First != first {
		return 0, nil, fmt.Errorf("the leader said the stream begins at %d but the "+
			"snapshot says %d", first, load.First)
	}
	return load.First, load.Anchors, nil
}

// applyStream applies framed records until the link breaks.
func (s *Server) applyStream(ctx context.Context, st *replicaState, nc net.Conn,
	br *bufio.Reader, from uint64, anchors persist.Anchors) error {

	replayer := command.NewReplayer(s)
	if anchors != nil {
		// Each shard of the shipped snapshot was taken at a different
		// instant, so early records belong to some shards and not others.
		// Once the stream passes the last anchor the filter stops excluding
		// anything, so it can simply stay installed.
		replayer.Filter(func(db, shard int) uint64 {
			return anchors[persist.ShardRef{DB: db, Shard: shard}]
		})
	}

	stream := persist.NewRecordStream(br, from)
	ackTicker := time.NewTicker(time.Second)
	defer ackTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ackTicker.C:
				writeCommand(nc, "REPLCONF", "ACK",
					strconv.FormatUint(st.offset.Load(), 10))
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !stream.Next() {
			if err := stream.Err(); err != nil {
				return err
			}
			return io.EOF
		}
		rec := stream.Record()
		if rec.Kind != persist.KindEffect {
			return fmt.Errorf("the leader sent a %d record on the stream", rec.Kind)
		}
		if err := replayer.Apply(rec.DB, rec.Offset, rec.Args); err != nil {
			// A record that does not apply means the two keyspaces have
			// already diverged. Carrying on would widen the gap silently.
			return fmt.Errorf("applying the leader's stream: %w", err)
		}
		st.offset.Store(stream.Offset())
		st.lastIO.Store(time.Now().Unix())
	}
}

func (r *replicaState) setReplID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replID = id
}

func (r *replicaState) lastReplID() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replID, r.replID != ""
}

func writeCommand(w io.Writer, args ...string) error {
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	_, err := w.Write(resp.EncodeCommand(nil, raw...))
	return err
}

func readLine(br *bufio.Reader) (string, error) {
	s, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// readBulkPayload reads a $<len>\r\n body with no trailing CRLF, which is how
// the snapshot travels: the record stream begins immediately after it.
func readBulkPayload(br *bufio.Reader) ([]byte, error) {
	head, err := readLine(br)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(head, "$") {
		return nil, fmt.Errorf("expected a snapshot payload, got %q", head)
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("bad snapshot payload header %q", head)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// replicaLink returns this server's link to its leader, or nil.
func (s *Server) replicaLink() *replicaState {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.replica
}

// startReplication applies the replicaof directive at startup.
//
// A failure here is logged rather than fatal: a leader that is not up yet is
// the ordinary case when a pair is started together, and the link retries on
// its own.
func (s *Server) startReplication(spec string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return
	}
	host, portStr, err := net.SplitHostPort(spec)
	if err != nil {
		// The directive is also written as two words in a config file.
		f := strings.Fields(spec)
		if len(f) != 2 {
			s.log.Error("replicaof is not host:port or \"host port\"", "value", spec)
			return
		}
		host, portStr = f[0], f[1]
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		s.log.Error("replicaof has an unusable port", "value", spec)
		return
	}
	if err := s.Follow(host, port); err != nil {
		s.log.Error("could not start replication", "leader", spec, "error", err)
	}
}

// stopReplication ends the link, if any, during shutdown.
func (s *Server) stopReplication() {
	s.replicaMu.Lock()
	st := s.replica
	s.replica = nil
	s.replicaMu.Unlock()
	if st != nil {
		st.cancel()
		<-st.done
	}
}
