package server

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"kestrel/command"
)

// defaultInfoSections is what INFO returns when no section is named.
var defaultInfoSections = []string{
	"server", "clients", "memory", "persistence", "stats",
	"replication", "cpu", "keyspace",
}

// allInfoSections adds the ones only returned on request or with "all".
var allInfoSections = append(append([]string{}, defaultInfoSections...),
	"commandstats", "latencystats")

// Info renders the INFO reply (FR-8.1).
func (s *Server) Info(sections []string) string {
	want := map[string]bool{}
	switch {
	case len(sections) == 0:
		for _, sec := range defaultInfoSections {
			want[sec] = true
		}
	case len(sections) == 1 && (sections[0] == "all" || sections[0] == "everything"):
		for _, sec := range allInfoSections {
			want[sec] = true
		}
	case len(sections) == 1 && sections[0] == "default":
		for _, sec := range defaultInfoSections {
			want[sec] = true
		}
	default:
		for _, sec := range sections {
			want[sec] = true
		}
	}

	var b strings.Builder
	for _, sec := range allInfoSections {
		if !want[sec] {
			continue
		}
		s.writeSection(&b, sec)
	}
	return b.String()
}

func (s *Server) writeSection(b *strings.Builder, name string) {
	switch name {
	case "server":
		s.infoServer(b)
	case "clients":
		s.infoClients(b)
	case "memory":
		s.infoMemory(b)
	case "persistence":
		s.infoPersistence(b)
	case "stats":
		s.infoStats(b)
	case "replication":
		s.infoReplication(b)
	case "cpu":
		s.infoCPU(b)
	case "keyspace":
		s.infoKeyspace(b)
	case "commandstats":
		s.infoCommandStats(b)
	case "latencystats":
		s.infoLatencyStats(b)
	}
}

func header(b *strings.Builder, name string) {
	if b.Len() > 0 {
		b.WriteString("\r\n")
	}
	b.WriteString("# ")
	b.WriteString(name)
	b.WriteString("\r\n")
}

func kv(b *strings.Builder, k string, v any) {
	fmt.Fprintf(b, "%s:%v\r\n", k, v)
}

func (s *Server) infoServer(b *strings.Builder) {
	snap := s.cfg.Snapshot()
	uptime := time.Since(s.startTime)
	header(b, "Server")
	kv(b, "kestrel_version", command.Version)
	kv(b, "resp_compatibility", command.RESPCompat)
	kv(b, "go_version", runtime.Version())
	kv(b, "os", runtime.GOOS+" "+runtime.GOARCH)
	kv(b, "process_id", os.Getpid())
	kv(b, "run_id", command.RunID())
	kv(b, "tcp_port", snap.Port)
	kv(b, "admin_port", snap.AdminPort)
	kv(b, "uptime_in_seconds", int64(uptime.Seconds()))
	kv(b, "uptime_in_days", int64(uptime.Hours()/24))
	kv(b, "config_file", snap.Source)
	kv(b, "shards", snap.Shards)
	kv(b, "executable", executablePath())
	// redis_version is reported so that client libraries which gate features
	// on it keep working. It is the compatibility target, not a claim to be
	// that software.
	kv(b, "redis_version", command.RESPCompat)
}

func executablePath() string {
	p, err := os.Executable()
	if err != nil {
		return "kestreld"
	}
	return p
}

func (s *Server) infoClients(b *strings.Builder) {
	snap := s.cfg.Snapshot()
	header(b, "Clients")
	kv(b, "connected_clients", s.clientCount())
	kv(b, "maxclients", snap.MaxClients)
	kv(b, "blocked_clients", 0)
	kv(b, "cluster_connections", 0)
}

func (s *Server) infoMemory(b *strings.Builder) {
	snap := s.cfg.Snapshot()
	var ms runtime.MemStats
	// ReadMemStats is expensive and is deliberately confined to INFO, never
	// to the command path (ADR-013).
	runtime.ReadMemStats(&ms)

	est := s.ks.MemoryEstimate()
	header(b, "Memory")
	kv(b, "used_memory", est)
	kv(b, "used_memory_human", humanBytes(est))
	kv(b, "used_memory_go_heap", ms.HeapAlloc)
	kv(b, "used_memory_rss", ms.Sys)
	kv(b, "used_memory_rss_human", humanBytes(int64(ms.Sys)))
	kv(b, "maxmemory", snap.MaxMemory)
	kv(b, "maxmemory_human", humanBytes(snap.MaxMemory))
	kv(b, "maxmemory_policy", snap.MaxMemoryPolicy)
	kv(b, "mem_fragmentation_ratio", ratio(float64(ms.Sys), float64(est)))
	kv(b, "mem_allocator", "go")
	// The estimate is approximate by construction; saying so in INFO is
	// cheaper than fielding the question later.
	kv(b, "used_memory_is_estimated", 1)
	kv(b, "gc_pause_total_ns", ms.PauseTotalNs)
	kv(b, "gc_num_gc", ms.NumGC)
}

func ratio(a, b float64) string {
	if b == 0 {
		return "0.00"
	}
	return fmt.Sprintf("%.2f", a/b)
}

func (s *Server) infoPersistence(b *strings.Builder) {
	snap := s.cfg.Snapshot()
	header(b, "Persistence")
	kv(b, "loading", boolInt(s.IsLoading()))
	kv(b, "appendonly", boolInt(snap.AppendOnly))
	kv(b, "appendfsync", snap.AppendFsync)
	kv(b, "changes_since_last_save", s.ks.Stats().Changes)

	p := s.persist
	kv(b, "aol_enabled", boolInt(p != nil))
	status, lastErr := "ok", ""
	if p != nil {
		if err := p.Err(); err != nil {
			status, lastErr = "err", err.Error()
		}
	}
	kv(b, "aol_last_write_status", status)
	if lastErr != "" {
		// The reason is reported as well as the status. A dashboard that can
		// only see "err" sends someone to read the process log, which is the
		// slowest possible way to learn that the disk is full.
		kv(b, "aol_last_write_error", lastErr)
	}
	st := p.stats()
	kv(b, "aol_current_size", st.Size)
	kv(b, "aol_stream_offset", st.Offset)
	kv(b, "aol_writes", st.Writes)
	kv(b, "aol_fsyncs", st.Syncs)
	kv(b, "aol_last_fsync_usec", st.LastSyncTime.Microseconds())
	if p != nil {
		kv(b, "aol_dir", p.dir)
		kv(b, "rdb_last_save_time", p.lastSave.Load())
	} else {
		kv(b, "rdb_last_save_time", 0)
	}
	kv(b, "snapshot_in_progress", 0)
}

func (s *Server) infoStats(b *strings.Builder) {
	gs, st := s.stats, s.ks.Stats()
	header(b, "Stats")
	kv(b, "total_connections_received", gs.TotalConnections.Load())
	kv(b, "total_commands_processed", gs.TotalCommands.Load())
	kv(b, "rejected_connections", gs.RejectedConnections.Load())
	kv(b, "unsupported_command_attempts", gs.UnsupportedCommands.Load())
	kv(b, "expired_keys", st.ExpiredKeys)
	kv(b, "evicted_keys", st.EvictedKeys)
	kv(b, "keyspace_hits", st.Hits)
	kv(b, "keyspace_misses", st.Misses)
	kv(b, "total_net_input_bytes", gs.TotalNetInput.Load())
	kv(b, "total_net_output_bytes", gs.TotalNetOutput.Load())
}

func (s *Server) infoReplication(b *strings.Builder) {
	header(b, "Replication")
	role := "master"
	if s.IsReplica() {
		role = "slave"
	}
	kv(b, "role", role)
	kv(b, "connected_slaves", 0)
	kv(b, "master_replid", command.RunID())
	kv(b, "master_repl_offset", 0)
	kv(b, "repl_backlog_active", 0)
}

func (s *Server) infoCPU(b *strings.Builder) {
	header(b, "CPU")
	kv(b, "num_cpu", runtime.NumCPU())
	kv(b, "gomaxprocs", runtime.GOMAXPROCS(0))
	kv(b, "num_goroutines", runtime.NumGoroutine())
}

func (s *Server) infoKeyspace(b *strings.Builder) {
	header(b, "Keyspace")
	for i := 0; i < s.ks.NumDatabases(); i++ {
		db := s.ks.DB(i)
		n := db.Size()
		if n == 0 {
			continue
		}
		fmt.Fprintf(b, "db%d:keys=%d,expires=%d,avg_ttl=0\r\n", i, n, db.TTLKeyCount())
	}
}

func (s *Server) infoCommandStats(b *strings.Builder) {
	header(b, "Commandstats")
	names, all := s.table.SortedCommandStats()
	for _, n := range names {
		cs := all[n]
		per := 0.0
		if cs.Calls > 0 {
			per = float64(cs.Micros) / float64(cs.Calls)
		}
		fmt.Fprintf(b, "cmdstat_%s:calls=%d,usec=%d,usec_per_call=%.2f,rejected_calls=%d,failed_calls=%d\r\n",
			strings.ToLower(strings.ReplaceAll(n, "|", "|")), cs.Calls, cs.Micros, per, cs.Rejected, cs.Errors)
	}
}

func (s *Server) infoLatencyStats(b *strings.Builder) {
	header(b, "Latencystats")
	names, all := s.table.SortedCommandStats()
	for _, n := range names {
		cs := all[n]
		if cs.Calls == 0 {
			continue
		}
		// Real percentiles arrive with the metrics histograms in M6; until
		// then the mean and the observed maximum are reported honestly
		// rather than being dressed up as quantiles.
		fmt.Fprintf(b, "latency_stats_%s:mean_usec=%.2f,max_usec=%d\r\n",
			strings.ToLower(n), float64(cs.Micros)/float64(cs.Calls), cs.MaxMicros)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%c", float64(n)/float64(div), "KMGTPE"[exp])
}
