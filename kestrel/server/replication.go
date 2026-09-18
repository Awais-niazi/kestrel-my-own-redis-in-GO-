package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"kestrel/command"
	"kestrel/persist"
)

// Replication, leader side.
//
// A replica is fed straight from the log. PSYNC either resumes from the
// offset the replica reports, which is a seek into a retained segment, or
// sends the current snapshot and then the log from that snapshot's earliest
// anchor. The second is not a special path: it is the same pair of files a
// restart reads, sent over a socket instead of opened from disk.
//
// Records go over the wire in their on-disk framing rather than as RESP
// commands. The replica therefore verifies the checksum the leader wrote,
// against the bytes the leader wrote, rather than one recomputed from
// arguments that have already been decoded and assumed correct.

// replicaLink is one connected replica, as the leader sees it.
type replicaLink struct {
	id     uint64
	addr   string
	port   int
	state  atomic.Value // string
	ack    *atomic.Uint64
	since  time.Time
	cancel context.CancelFunc
}

func (r *replicaLink) status() string {
	if v, ok := r.state.Load().(string); ok {
		return v
	}
	return "connecting"
}

// ReplID identifies this server's replication history. A replica that
// reconnects quoting a different one cannot be resumed, because the offsets
// it holds refer to a stream this server did not write.
func (s *Server) ReplID() string { return command.RunID() }

// registerReplica adds a link and returns it.
func (s *Server) registerReplica(l *replicaLink) {
	s.replicasMu.Lock()
	defer s.replicasMu.Unlock()
	if s.replicas == nil {
		s.replicas = make(map[uint64]*replicaLink)
	}
	s.replicas[l.id] = l
}

func (s *Server) unregisterReplica(id uint64) {
	s.replicasMu.Lock()
	defer s.replicasMu.Unlock()
	delete(s.replicas, id)
}

// replicaLinks returns the connected replicas, ordered by id so that INFO is
// stable between calls.
func (s *Server) replicaLinks() []*replicaLink {
	s.replicasMu.Lock()
	defer s.replicasMu.Unlock()
	out := make([]*replicaLink, 0, len(s.replicas))
	for _, l := range s.replicas {
		out = append(out, l)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].id < out[j-1].id; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// serveReplica takes a connection out of the command loop and feeds it the
// replication stream. It returns when the link ends.
func (s *Server) serveReplica(c *connection, req *command.PSyncRequest) {
	p := s.persist
	if p == nil {
		// Without a log there is nothing to stream and nothing to resume
		// from. Saying so is better than a link that silently never sends.
		fmt.Fprintf(c.nc, "-ERR replication requires appendonly yes\r\n")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	link := &replicaLink{
		id: c.cl.ID, addr: c.cl.Addr, port: req.ListeningPort,
		ack: &c.cl.ReplicaAck, since: time.Now(), cancel: cancel,
	}
	link.state.Store("sync")
	s.registerReplica(link)
	defer s.unregisterReplica(link.id)

	from, err := s.beginSync(c, p, req)
	if err != nil {
		s.log.Warn("replica synchronisation failed", "replica", c.cl.Addr, "error", err)
		return
	}
	link.state.Store("online")
	s.log.Info("replica online", "replica", c.cl.Addr, "from_offset", from)

	// Acknowledgements arrive on the same connection, in the other
	// direction, while the stream is being written.
	go s.readReplicaAcks(ctx, c, link)

	if err := s.streamTo(ctx, c, p, from); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Info("replica link ended", "replica", c.cl.Addr, "reason", err)
	}
}

// beginSync answers the PSYNC and returns the offset the stream starts at.
func (s *Server) beginSync(c *connection, p *persistence, req *command.PSyncRequest) (uint64, error) {
	oldest, newest := p.log.OldestOffset(), p.log.Offset()

	if req.Known && req.ReplID == s.ReplID() && req.Offset >= oldest && req.Offset <= newest {
		// Everything the replica is missing is still on disk, so the link
		// resumes with no snapshot transfer at all.
		if _, err := fmt.Fprintf(c.nc, "+CONTINUE %s\r\n", s.ReplID()); err != nil {
			return 0, err
		}
		return req.Offset, nil
	}

	// A full synchronisation sends the snapshot and then the log from its
	// earliest anchor. Pruning never removes a segment below that anchor, so
	// the pair is always consistent and no fresh snapshot is needed for each
	// replica -- only for the first, if none has ever been taken.
	if _, err := os.Stat(p.snapPath); errors.Is(err, os.ErrNotExist) {
		s.log.Info("taking a snapshot for a replica's first synchronisation",
			"replica", c.cl.Addr)
		if err := s.snapshotAndLog(p, "replica sync"); err != nil {
			return 0, err
		}
	}

	f, err := os.Open(p.snapPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	first, err := snapshotFirstAnchor(p.snapPath)
	if err != nil {
		return 0, err
	}

	if _, err := fmt.Fprintf(c.nc, "+FULLRESYNC %s %d\r\n$%d\r\n",
		s.ReplID(), first, fi.Size()); err != nil {
		return 0, err
	}
	if _, err := io.Copy(c.nc, f); err != nil {
		return 0, err
	}
	return first, nil
}

// snapshotFirstAnchor reads the window a snapshot covers without loading it.
func snapshotFirstAnchor(path string) (uint64, error) {
	r, f, err := persist.OpenSnapshotReader(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	for r.Next() {
		rec := r.Record()
		if rec.Kind != persist.KindAnchor {
			continue
		}
		_, _, off, err := persist.ParseAnchor(rec.Args)
		return off, err
	}
	if err := r.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("persist: %s has no anchors", path)
}

// streamTo forwards records from the log to a replica until the link ends.
func (s *Server) streamTo(ctx context.Context, c *connection, p *persistence, from uint64) error {
	fl, err := persist.Follow(p.log, from)
	if err != nil {
		return err
	}
	defer fl.Close()
	fl.KeepRaw(true)

	for {
		rec, err := fl.Next(ctx)
		if err != nil {
			return err
		}
		if _, err := c.nc.Write(rec.Raw); err != nil {
			return err
		}
	}
}

// readReplicaAcks consumes REPLCONF ACK from a replica while the stream runs
// in the other direction.
func (s *Server) readReplicaAcks(ctx context.Context, c *connection, link *replicaLink) {
	defer link.cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		default:
		}
		c.nc.SetReadDeadline(time.Time{})
		args, err := c.rd.ReadCommand()
		if err != nil {
			return
		}
		if len(args) >= 3 && strings.EqualFold(string(args[0]), "REPLCONF") &&
			strings.EqualFold(string(args[1]), "ACK") {
			if n, err := strconv.ParseUint(string(args[2]), 10, 64); err == nil {
				link.ack.Store(n)
			}
		}
	}
}
