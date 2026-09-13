package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strings"
	"time"

	"kestrel/config"
)

// adminServer is the separate HTTP listener carrying metrics, health checks
// and, when explicitly enabled, profiling endpoints (FR-8.2, FR-8.5).
//
// It is deliberately a different port from the data protocol so that it can
// be exposed to a monitoring network without exposing the datastore.
type adminServer struct {
	srv  *Server
	http *http.Server
	ln   net.Listener
	addr string
}

func newAdminServer(s *Server, snap *config.Values) *adminServer {
	mux := http.NewServeMux()
	a := &adminServer{srv: s}

	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/ready", a.handleReady)
	if snap.MetricsEnabled {
		mux.HandleFunc("/metrics", a.handleMetrics)
	}
	if snap.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	a.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	a.addr = net.JoinHostPort(snap.Bind, fmt.Sprint(snap.AdminPort))
	return a
}

func (a *adminServer) start() error {
	ln, err := net.Listen("tcp", a.addr)
	if err != nil {
		return fmt.Errorf("listening on admin port: %w", err)
	}
	a.ln = ln
	a.srv.log.Info("admin listening", "addr", ln.Addr().String())
	a.srv.workers.Add(1)
	go func() {
		defer a.srv.workers.Done()
		if err := a.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.srv.log.Error("admin server stopped", "err", err)
		}
	}()
	return nil
}

func (a *adminServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.http.Shutdown(ctx)
}

// handleHealth is the liveness probe: it answers as long as the process can
// serve HTTP at all.
func (a *adminServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "OK")
}

// handleReady is the readiness probe, gated on the dataset being loaded.
func (a *adminServer) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if a.srv.IsLoading() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "loading")
		return
	}
	fmt.Fprintln(w, "ready")
}

// handleMetrics writes the Prometheus text exposition format.
//
// The format is written by hand here rather than through a client library.
// ADR-015 approves prometheus/client_golang for this endpoint, and the M6
// metric set -- histograms for command latency, fsync latency and GC pauses
// -- is where that dependency earns its place. Until those exist, a few
// counters and gauges do not justify the tree.
func (a *adminServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s := a.srv
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := s.ks.Stats()

	var b strings.Builder
	metric := func(name, help, typ string, value any, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %v\n", name, value)
			return
		}
		fmt.Fprintf(&b, "%s{%s} %v\n", name, strings.Join(labels, ","), value)
	}

	metric("kestrel_up", "Whether the server is serving.", "gauge", 1)
	metric("kestrel_uptime_seconds", "Seconds since startup.", "gauge",
		int64(time.Since(s.startTime).Seconds()))
	metric("kestrel_connected_clients", "Currently connected clients.", "gauge", s.clientCount())
	metric("kestrel_connections_total", "Connections accepted since startup.", "counter",
		s.stats.TotalConnections.Load())
	metric("kestrel_connections_rejected_total", "Connections refused.", "counter",
		s.stats.RejectedConnections.Load())
	metric("kestrel_commands_total", "Commands processed.", "counter", s.stats.TotalCommands.Load())
	metric("kestrel_unsupported_commands_total",
		"Attempts to use a command this version does not implement.", "counter",
		s.stats.UnsupportedCommands.Load())
	metric("kestrel_keyspace_hits_total", "Successful key lookups.", "counter", st.Hits)
	metric("kestrel_keyspace_misses_total", "Failed key lookups.", "counter", st.Misses)
	metric("kestrel_expired_keys_total", "Keys removed by expiration.", "counter",
		st.ExpiredKeys)
	metric("kestrel_evicted_keys_total", "Keys removed by eviction.", "counter",
		st.EvictedKeys)
	metric("kestrel_memory_estimated_bytes",
		"Estimated logical dataset size. Approximate by design; see ADR-013.", "gauge",
		s.ks.MemoryEstimate())
	metric("kestrel_go_heap_bytes", "Go heap in use.", "gauge", ms.HeapAlloc)
	metric("kestrel_go_sys_bytes", "Memory obtained from the OS.", "gauge", ms.Sys)
	metric("kestrel_gc_pause_seconds_total", "Cumulative GC stop-the-world time.", "counter",
		float64(ms.PauseTotalNs)/1e9)
	metric("kestrel_gc_cycles_total", "Completed GC cycles.", "counter", ms.NumGC)
	metric("kestrel_goroutines", "Live goroutines.", "gauge", runtime.NumGoroutine())

	// Per-database key counts.
	fmt.Fprintf(&b, "# HELP kestrel_db_keys Keys per database.\n# TYPE kestrel_db_keys gauge\n")
	for i := 0; i < s.ks.NumDatabases(); i++ {
		if n := s.ks.DB(i).Size(); n > 0 {
			fmt.Fprintf(&b, "kestrel_db_keys{db=\"%d\"} %d\n", i, n)
		}
	}

	// Per-command counters.
	names, stats := s.table.SortedCommandStats()
	fmt.Fprintf(&b, "# HELP kestrel_command_calls_total Calls per command.\n"+
		"# TYPE kestrel_command_calls_total counter\n")
	for _, n := range names {
		fmt.Fprintf(&b, "kestrel_command_calls_total{command=%q} %d\n",
			strings.ToLower(n), stats[n].Calls)
	}
	fmt.Fprintf(&b, "# HELP kestrel_command_seconds_total Time spent per command.\n"+
		"# TYPE kestrel_command_seconds_total counter\n")
	for _, n := range names {
		fmt.Fprintf(&b, "kestrel_command_seconds_total{command=%q} %f\n",
			strings.ToLower(n), float64(stats[n].Micros)/1e6)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
