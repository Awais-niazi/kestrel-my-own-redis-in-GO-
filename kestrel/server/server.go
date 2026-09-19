// Package server wires the listeners, connections, and background workers
// around the command layer. It is the only package that knows about sockets.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"kestrel/command"
	"kestrel/config"
	"kestrel/engine"
)

// EffectLog receives the canonical effects of every write, in order.
//
// The append log and the replication backlog both implement it (M3 and M4);
// until then the server installs a counting no-op, so that the propagation
// path is exercised from the first milestone rather than being bolted on
// once its bugs are expensive.
type EffectLog interface {
	Append(db int, args [][]byte)
}

// discardLog counts effects and throws them away.
type discardLog struct{ count atomic.Int64 }

func (d *discardLog) Append(db int, args [][]byte) { d.count.Add(1) }

// Server is a running kestreld instance.
type Server struct {
	cfg   *config.Config
	ks    *engine.Keyspace
	table *command.Table
	stats *command.Stats
	log   *slog.Logger

	startTime time.Time
	effects   atomic.Pointer[EffectLog]

	listeners []net.Listener
	admin     *adminServer

	clientsMu sync.Mutex
	clients   map[uint64]*connection
	nextID    atomic.Uint64

	loading atomic.Bool
	persist *persistence

	replicasMu sync.Mutex
	replicas   map[uint64]*replicaLink

	// replicaMu guards the link to this server's own leader, when it has
	// one. It is separate from replicasMu: that one is about the replicas
	// following this server, this one about the server this one follows.
	replicaMu sync.Mutex
	replica   *replicaState
	isReplica atomic.Bool

	pubsub   *command.PubSub
	watchers *command.Watchers
	blocked  *command.Blocked

	quit         chan struct{}
	shutdownOnce sync.Once
	shutdownErr  error
	conns        sync.WaitGroup
	workers      sync.WaitGroup
}

// New builds a server from a validated configuration.
func New(cfg *config.Config) (*Server, error) {
	snap := cfg.Snapshot()

	table, err := command.NewTable(snap.RenamedCommands)
	if err != nil {
		return nil, err
	}

	ks := engine.New(engine.Options{
		Databases:              snap.Databases,
		Shards:                 snap.Shards,
		MaxStringLength:        int(snap.ProtoMaxBulkLen),
		ActiveExpire:           snap.ActiveExpire,
		ActiveExpireSampleSize: snap.ActiveExpireSampleSize,
		ActiveExpireCPUPercent: snap.ActiveExpireCPUPercent,
		CachedClock:            true,
		Encoding:               encodingThresholds(snap),
	})

	s := &Server{
		cfg:       cfg,
		ks:        ks,
		table:     table,
		stats:     command.NewStats(snap.SlowlogMaxLen),
		log:       newLogger(snap.LogLevel, snap.LogFormat),
		startTime: time.Now(),
		clients:   make(map[uint64]*connection),
		quit:      make(chan struct{}),
		pubsub:    command.NewPubSub(),
		watchers:  command.NewWatchers(),
		blocked:   command.NewBlocked(),
	}
	var el EffectLog = &discardLog{}
	s.effects.Store(&el)

	// Effects the engine produces on its own -- today only the DEL from a
	// reaped key -- travel the same path as command effects (FR-3.4).
	ks.SetEffectSink(effectSink{s})
	// Both WATCH and the blocking commands learn about writes from the same
	// hook, which the engine calls under the shard lock of whichever
	// goroutine performed the write.
	ks.SetKeyWatcher(keyWatchers{s.watchers, s.blocked})

	// The GC is told the ceiling derived from maxmemory, so that pressure
	// shows up as slower collection rather than as an OOM kill (ADR-013).
	applyMemoryLimit(snap.MaxMemory, snap.MemoryLimitOverheadFactor, s.log)
	if snap.GOGC > 0 {
		debug.SetGCPercent(snap.GOGC)
	}
	return s, nil
}

// encodingThresholds maps the configuration onto the engine's promotion
// thresholds (ADR-006).
func encodingThresholds(snap *config.Values) engine.Thresholds {
	return engine.Thresholds{
		HashMaxListpackEntries: snap.HashMaxListpackEntries,
		HashMaxListpackValue:   snap.HashMaxListpackValue,
		ListMaxListpackSize:    snap.ListMaxListpackSize,
		ListMaxListpackValue:   snap.ListMaxListpackValue,
		SetMaxIntsetEntries:    snap.SetMaxIntsetEntries,
		SetMaxListpackEntries:  snap.SetMaxListpackEntries,
		SetMaxListpackValue:    snap.SetMaxListpackValue,
		ZSetMaxListpackEntries: snap.ZsetMaxListpackEntries,
		ZSetMaxListpackValue:   snap.ZsetMaxListpackValue,
	}
}

// ApplyRuntimeConfig pushes configuration that other subsystems cache. The
// CONFIG SET handler calls it so that a threshold change takes effect
// immediately rather than at the next restart.
func (s *Server) ApplyRuntimeConfig() {
	snap := s.cfg.Snapshot()
	s.stats.Slowlog.SetCapacity(snap.SlowlogMaxLen)
	s.ks.SetThresholds(encodingThresholds(snap))
}

type effectSink struct{ s *Server }

func (e effectSink) Effect(db int, args ...[]byte) { e.s.Propagate(db, args...) }

func applyMemoryLimit(maxMemory int64, factor float64, log *slog.Logger) {
	if maxMemory <= 0 {
		return
	}
	if factor < 1 {
		factor = 1
	}
	limit := int64(float64(maxMemory) * factor)
	debug.SetMemoryLimit(limit)
	log.Info("soft memory limit applied",
		"maxmemory", maxMemory, "overhead_factor", factor, "go_memory_limit", limit)
}

// Host interface (see command.Host).

// Keyspace returns the data store.
func (s *Server) Keyspace() *engine.Keyspace { return s.ks }

// Config returns the live configuration.
func (s *Server) Config() *config.Config { return s.cfg }

// Commands returns the command table.
func (s *Server) Commands() *command.Table { return s.table }

// Stats returns the shared counters.
func (s *Server) Stats() *command.Stats { return s.stats }

// StartTime reports when the server began serving.
func (s *Server) StartTime() time.Time { return s.startTime }

// IsReplica reports whether this node follows a leader.
func (s *Server) IsReplica() bool { return s.isReplica.Load() }

// PubSub returns the subscription registry.
func (s *Server) PubSub() *command.PubSub { return s.pubsub }

// Watchers returns the WATCH registry.
func (s *Server) Watchers() *command.Watchers { return s.watchers }

// Blocked returns the registry of clients waiting on keys.
func (s *Server) Blocked() *command.Blocked { return s.blocked }

// Quit is closed when the server begins shutting down.
func (s *Server) Quit() <-chan struct{} { return s.quit }

// keyWatchers fans the engine's single key-modification hook out to
// everything that needs it. It is called under a shard lock, so each
// listener does no more than a map lookup.
type keyWatchers []engine.KeyWatcher

func (k keyWatchers) KeyModified(db int, key []byte) {
	for _, w := range k {
		w.KeyModified(db, key)
	}
}

// IsLoading reports whether the dataset is still being read from disk.
func (s *Server) IsLoading() bool { return s.loading.Load() }

// Logger returns the structured logger.
func (s *Server) Logger() *slog.Logger { return s.log }

// SetEffectLog installs the destination for write effects. The append log
// (M3) and the replication backlog (M4) attach here.
func (s *Server) SetEffectLog(l EffectLog) { s.effects.Store(&l) }

// Propagate hands a canonical effect to the log and the replication stream.
func (s *Server) Propagate(db int, args ...[]byte) {
	if p := s.effects.Load(); p != nil {
		(*p).Append(db, args)
	}
}

// Shutdown asks the server to stop and returns once the listeners are closed.
func (s *Server) Shutdown(save bool) error {
	s.shutdownOnce.Do(func() {
		s.log.Info("shutdown requested", "save", save)
		close(s.quit)
		for _, l := range s.listeners {
			l.Close()
		}
	})
	return s.shutdownErr
}

// Serve opens the listeners and blocks until the server is shut down or ctx
// is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	snap := s.cfg.Snapshot()

	if snap.Port != 0 {
		l, err := listen(snap.Bind, snap.Port, snap.TCPBacklog)
		if err != nil {
			return err
		}
		s.listeners = append(s.listeners, l)
		s.log.Info("listening", "addr", l.Addr().String(), "tls", false)
	}
	if snap.TLSPort != 0 {
		cfgTLS, err := buildTLSConfig(snap)
		if err != nil {
			return err
		}
		raw, err := listen(snap.Bind, snap.TLSPort, snap.TCPBacklog)
		if err != nil {
			return err
		}
		l := tls.NewListener(raw, cfgTLS)
		s.listeners = append(s.listeners, l)
		s.log.Info("listening", "addr", raw.Addr().String(), "tls", true)
	}
	if len(s.listeners) == 0 {
		return errors.New("no listeners configured")
	}

	if snap.AdminPort != 0 {
		s.admin = newAdminServer(s, snap)
		if err := s.admin.start(); err != nil {
			return err
		}
	}

	s.warnIfExposed(snap)

	// Recovery runs after the listeners exist and before any of them is
	// accepted from. The ports are therefore already bound, so a client that
	// connects during a long replay waits in the backlog rather than being
	// refused, and the admin server's readiness probe reports the truth
	// while it happens.
	p, err := s.openPersistence(snap)
	if err != nil {
		return err
	}
	s.persist = p
	if p == nil {
		s.log.Info("persistence is disabled: data is held in memory only and " +
			"is lost on restart")
	} else {
		s.loading.Store(true)
		err := s.restore(p, snap)
		s.loading.Store(false)
		if err != nil {
			return err
		}
		var el EffectLog = p
		s.SetEffectLog(el)
		// The scheduling baselines start from the state recovery left, so
		// the first snapshot is due an interval from now rather than
		// immediately.
		p.changesAtSave.Store(s.ks.Stats().Changes)
		p.sizeAtSave.Store(p.log.Stats().Size)
		s.workers.Add(1)
		go s.maintenance(p)
		s.startReplication(snap.ReplicaOf)
		s.log.Info("persistence is on", "dir", p.dir,
			"appendfsync", snap.AppendFsync,
			"snapshot_interval_seconds", snap.SnapshotInterval)
	}

	for _, l := range s.listeners {
		s.workers.Add(1)
		go s.acceptLoop(l)
	}

	s.workers.Add(1)
	go s.idleReaper()

	select {
	case <-ctx.Done():
		s.Shutdown(true)
	case <-s.quit:
	}
	return s.drain(snap)
}

// warnIfExposed says plainly when the server is reachable from off-box
// without a password, rather than letting that be discovered later.
func (s *Server) warnIfExposed(snap *config.Values) {
	if snap.RequirePass != "" {
		return
	}
	if snap.ProtectedMode {
		s.log.Info("protected mode is on: connections from non-loopback addresses will be refused " +
			"because no password is set")
		return
	}
	if snap.Bind != "127.0.0.1" && snap.Bind != "localhost" && snap.Bind != "::1" {
		s.log.Warn("server is bound to a non-loopback address with no password and protected mode off",
			"bind", snap.Bind)
	}
}

// drain implements the graceful shutdown sequence of NFR-3: stop accepting,
// let in-flight commands finish, flush and sync durable state, then exit.
func (s *Server) drain(snap *config.Values) error {
	timeout := 30 * time.Second
	deadline := time.Now().Add(timeout)

	for _, l := range s.listeners {
		l.Close()
	}
	if s.admin != nil {
		s.admin.stop()
	}

	s.unblockClients()
	s.stopReplication()

	done := make(chan struct{})
	go func() {
		s.conns.Wait()
		s.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		s.log.Warn("shutdown timed out waiting for connections to drain", "timeout", timeout)
	}

	s.ks.Close()
	if s.persist != nil {
		// The log is closed after the connections have drained, so every
		// effect that was acknowledged is in it, and a close that cannot
		// force the file reports rather than swallowing the failure: this is
		// exactly the case where an operator believes a clean shutdown was
		// durable.
		if err := s.persist.Close(); err != nil {
			s.log.Error("closing the append log failed; the last writes may not "+
				"be on disk", "error", err)
			s.shutdownErr = err
		}
	}
	s.log.Info("shutdown complete",
		"uptime_seconds", int64(time.Since(s.startTime).Seconds()))
	return nil
}

// unblockClients wakes every connection that is parked in a read so that it
// notices the closed quit channel and returns.
//
// It deliberately does not touch Client.CloseAfterReply: that field belongs
// to the connection's own goroutine, and writing it from here would be a
// data race on state the command layer assumes is single-threaded.
func (s *Server) unblockClients() {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for _, c := range s.clients {
		c.nc.SetReadDeadline(time.Now())
	}
}

func listen(bind string, port, backlog int) (net.Listener, error) {
	addr := net.JoinHostPort(bind, fmt.Sprint(port))
	lc := net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return l, nil
}

func buildTLSConfig(snap *config.Values) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(snap.TLSCertFile, snap.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS key pair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if snap.TLSCACertFile != "" {
		pem, err := os.ReadFile(snap.TLSCACertFile)
		if err != nil {
			return nil, fmt.Errorf("reading TLS CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("TLS CA file contains no usable certificates")
		}
		cfg.ClientCAs = pool
	}
	if snap.TLSAuthClients {
		if cfg.ClientCAs == nil {
			return nil, errors.New("tls-auth-clients requires tls-ca-cert-file")
		}
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func newLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
