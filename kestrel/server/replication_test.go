package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"kestrel/persist"
	"kestrel/resp"
)

// fakeReplica speaks the leader's side of the replication protocol by hand,
// so the tests exercise the wire exchange rather than an internal call.
type fakeReplica struct {
	t   *testing.T
	nc  net.Conn
	br  *bufio.Reader
	rec *persist.Reader

	ReplID   string
	Offset   uint64
	Snapshot []byte
	Full     bool
}

func dialReplica(t *testing.T, ts *testServer, replID string, offset uint64, port int) *fakeReplica {
	t.Helper()
	nc, err := net.Dial("tcp", ts.addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	nc.SetDeadline(time.Now().Add(20 * time.Second))
	r := &fakeReplica{t: t, nc: nc, br: bufio.NewReader(nc)}

	r.send("REPLCONF", "listening-port", strconv.Itoa(port))
	if got := r.line(); got != "+OK" {
		t.Fatalf("REPLCONF returned %q", got)
	}
	r.send("PSYNC", replID, strconv.FormatUint(offset, 10))

	head := r.line()
	switch {
	case strings.HasPrefix(head, "+CONTINUE "):
		r.ReplID = strings.TrimSpace(strings.TrimPrefix(head, "+CONTINUE "))
		r.Offset = offset
	case strings.HasPrefix(head, "+FULLRESYNC "):
		f := strings.Fields(head)
		if len(f) != 3 {
			t.Fatalf("malformed FULLRESYNC: %q", head)
		}
		r.Full, r.ReplID = true, f[1]
		r.Offset, err = strconv.ParseUint(f[2], 10, 64)
		if err != nil {
			t.Fatalf("FULLRESYNC offset %q: %v", f[2], err)
		}
		r.Snapshot = r.bulk()
	default:
		t.Fatalf("PSYNC returned %q", head)
	}
	r.rec = persist.NewRecordStream(r.br, r.Offset)
	return r
}

func (r *fakeReplica) send(args ...string) {
	r.t.Helper()
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	if _, err := r.nc.Write(resp.EncodeCommand(nil, raw...)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fakeReplica) line() string {
	r.t.Helper()
	s, err := r.br.ReadString('\n')
	if err != nil {
		r.t.Fatalf("reading a line: %v", err)
	}
	return strings.TrimRight(s, "\r\n")
}

// bulk reads a $<len>\r\n payload, which carries the snapshot. There is no
// trailing CRLF: the record stream begins immediately after the bytes.
func (r *fakeReplica) bulk() []byte {
	r.t.Helper()
	head := r.line()
	if !strings.HasPrefix(head, "$") {
		r.t.Fatalf("expected a bulk header, got %q", head)
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil || n < 0 {
		r.t.Fatalf("bad bulk header %q", head)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		r.t.Fatalf("reading the snapshot payload: %v", err)
	}
	return buf
}

// next reads one streamed record.
func (r *fakeReplica) next() (string, uint64) {
	r.t.Helper()
	if !r.rec.Next() {
		r.t.Fatalf("stream ended: %v", r.rec.Err())
	}
	rec := r.rec.Record()
	parts := make([]string, len(rec.Args))
	for i, a := range rec.Args {
		parts[i] = string(a)
	}
	return fmt.Sprintf("%d:%s", rec.DB, strings.Join(parts, " ")), r.rec.Offset()
}

func TestPsyncFullResync(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "before", "sync")
	c.do("SAVE")

	r := dialReplica(t, ts, "?", 0, 7001)
	if !r.Full {
		t.Fatal("a replica with no history got a partial resynchronisation")
	}
	if r.ReplID != ts.ReplID() {
		t.Errorf("replid is %q, want %q", r.ReplID, ts.ReplID())
	}
	if len(r.Snapshot) == 0 {
		t.Fatal("no snapshot was sent")
	}

	// The payload must be a real snapshot, holding the data written before.
	applied := replayShippedSnapshot(t, r.Snapshot)
	if !strings.Contains(strings.Join(applied, "|"), "SET before sync") {
		t.Errorf("the shipped snapshot does not hold the data: %v", applied)
	}

	// Writes after the handshake arrive on the stream.
	c.do("SET", "after", "sync")
	got, _ := r.next()
	if got != "0:SET after sync" {
		t.Errorf("streamed %q", got)
	}
}

// replayShippedSnapshot writes the bytes a replica received to a file and
// loads them, which is what a real replica does with them.
func replayShippedSnapshot(t *testing.T, body []byte) []string {
	t.Helper()
	path := t.TempDir() + "/shipped.snapshot"
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out []string
	load, err := persist.LoadSnapshot(path, applierFunc(func(db int, off uint64, args [][]byte) error {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = string(a)
		}
		out = append(out, strings.Join(parts, " "))
		return nil
	}))
	if err != nil {
		t.Fatalf("the shipped snapshot does not load: %v", err)
	}
	if load.Records == 0 {
		t.Error("the shipped snapshot is empty")
	}
	return out
}

type applierFunc func(db int, off uint64, args [][]byte) error

func (f applierFunc) Apply(db int, off uint64, args [][]byte) error { return f(db, off, args) }

// TestPsyncPartialResync is the case the log-as-backlog design exists for: a
// replica that reconnects with a known offset resumes with no snapshot.
func TestPsyncPartialResync(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")
	at := ts.replicationOffset()
	c.do("SET", "b", "2")
	c.do("SET", "c", "3")

	r := dialReplica(t, ts, ts.ReplID(), at, 7002)
	if r.Full {
		t.Fatal("a replica with a retained offset was given a full resynchronisation")
	}
	if len(r.Snapshot) != 0 {
		t.Error("a partial resynchronisation shipped a snapshot")
	}

	// It picks up exactly the records it was missing, in order.
	for _, want := range []string{"0:SET b 2", "0:SET c 3"} {
		if got, _ := r.next(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	c.do("SET", "d", "4")
	if got, _ := r.next(); got != "0:SET d 4" {
		t.Errorf("live record is %q", got)
	}
}

// TestPsyncRefusesPartialAfterCompaction is the other half: once the records
// have been unlinked the leader must say so by sending a full resync rather
// than a stream with a hole in it.
func TestPsyncRefusesPartialAfterCompaction(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")
	stale := ts.replicationOffset()
	for i := 0; i < 200; i++ {
		c.do("SET", fmt.Sprintf("k%d", i%10), fmt.Sprint(i))
	}
	c.do("SAVE")
	c.do("SET", "after", "compaction")
	c.do("SAVE")

	if oldest := ts.persist.log.OldestOffset(); oldest <= stale {
		t.Skipf("compaction did not pass the stale offset (%d <= %d)", oldest, stale)
	}
	r := dialReplica(t, ts, ts.ReplID(), stale, 7003)
	if !r.Full {
		t.Error("a replica asking for compacted records was resumed partially")
	}
}

// TestPsyncWrongReplIDIsFullResync guards against resuming against a stream
// this server never wrote.
func TestPsyncWrongReplIDIsFullResync(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")

	r := dialReplica(t, ts, "0000000000000000000000000000000000000000",
		ts.replicationOffset(), 7004)
	if !r.Full {
		t.Error("a replica quoting another leader's history was resumed partially")
	}
}

func TestInfoReplicationListsReplicas(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")
	r := dialReplica(t, ts, "?", 0, 7005)
	_ = r

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info := text(c.do("INFO", "replication"))
		if strings.Contains(info, "connected_slaves:1") && strings.Contains(info, "state=online") {
			if !strings.Contains(info, "port=7005") {
				t.Errorf("the announced listening port is missing:\n%s", info)
			}
			if !strings.Contains(info, "master_replid:"+ts.ReplID()) {
				t.Errorf("replid missing:\n%s", info)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the replica never showed up in INFO:\n%s", text(c.do("INFO", "replication")))
}

// TestReplicaAckIsRecorded covers the reverse direction on a link that is
// already carrying a stream.
func TestReplicaAckIsRecorded(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")
	r := dialReplica(t, ts, "?", 0, 7006)

	c.do("SET", "b", "2")
	_, off := r.next()
	r.send("REPLCONF", "ACK", strconv.FormatUint(off, 10))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		links := ts.replicaLinks()
		if len(links) == 1 && links[0].ack.Load() == off {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the acknowledgement was never recorded")
}

func TestReplicationRefusedWithoutPersistence(t *testing.T) {
	ts := startServer(t)
	nc, err := net.Dial("tcp", ts.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	nc.Write(resp.EncodeCommand(nil, []byte("PSYNC"), []byte("?"), []byte("0")))

	br := bufio.NewReader(nc)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "appendonly") {
		t.Errorf("got %q, want an error naming the reason", line)
	}
}

// TestReplicaLinkSurvivesARoll checks that compaction under a live replica
// does not break the stream.
func TestReplicaLinkSurvivesARoll(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "a", "1")
	r := dialReplica(t, ts, "?", 0, 7007)

	c.do("SET", "before-roll", "1")
	if got, _ := r.next(); got != "0:SET before-roll 1" {
		t.Fatalf("got %q", got)
	}
	c.do("SAVE") // rolls and prunes underneath the link
	c.do("SET", "after-roll", "2")
	if got, _ := r.next(); got != "0:SET after-roll 2" {
		t.Errorf("the link lost its place across a roll: %q", got)
	}
}
