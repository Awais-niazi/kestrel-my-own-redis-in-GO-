package resp

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, input string) [][]string {
	t.Helper()
	rd := NewReader(strings.NewReader(input), DefaultLimits())
	var out [][]string
	for {
		args, err := rd.ReadCommand()
		if err == io.EOF || err == io.ErrNoProgress {
			return out
		}
		if err != nil {
			t.Fatalf("ReadCommand(%q): %v", input, err)
		}
		cmd := make([]string, len(args))
		for i, a := range args {
			cmd[i] = string(a)
		}
		out = append(out, cmd)
	}
}

func TestReadMultiBulk(t *testing.T) {
	got := readAll(t, "*2\r\n$4\r\nECHO\r\n$5\r\nhello\r\n")
	want := [][]string{{"ECHO", "hello"}}
	if !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPipelining(t *testing.T) {
	in := "*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nPING\r\n*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"
	got := readAll(t, in)
	if len(got) != 3 || got[2][1] != "k" {
		t.Fatalf("got %v", got)
	}
}

func TestEmptyAndNullCommandsSkipped(t *testing.T) {
	got := readAll(t, "*0\r\n*-1\r\n\r\n*1\r\n$4\r\nPING\r\n")
	if len(got) != 1 || got[0][0] != "PING" {
		t.Fatalf("got %v", got)
	}
}

func TestBinarySafeArguments(t *testing.T) {
	val := "a\r\nb\x00c"
	in := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$6\r\n" + val + "\r\n"
	got := readAll(t, in)
	if len(got) != 1 || got[0][2] != val {
		t.Fatalf("got %q", got)
	}
}

func TestSplitReads(t *testing.T) {
	// Deliver the command one byte at a time; the parser must resume cleanly.
	full := "*2\r\n$3\r\nGET\r\n$5\r\nmykey\r\n"
	rd := NewReader(iotest(full), DefaultLimits())
	args, err := rd.ReadCommand()
	if err != nil {
		t.Fatal(err)
	}
	if string(args[0]) != "GET" || string(args[1]) != "mykey" {
		t.Fatalf("got %q %q", args[0], args[1])
	}
}

// iotest returns a reader that yields one byte per Read call.
func iotest(s string) io.Reader { return &dribble{s: s} }

type dribble struct {
	s string
	i int
}

func (d *dribble) Read(p []byte) (int, error) {
	if d.i >= len(d.s) {
		return 0, io.EOF
	}
	p[0] = d.s[d.i]
	d.i++
	return 1, nil
}

func TestInlineCommands(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"PING\r\n", []string{"PING"}},
		{"SET k v\n", []string{"SET", "k", "v"}},
		{"SET  k   \"hello world\"\r\n", []string{"SET", "k", "hello world"}},
		{`SET k "a\nb"` + "\r\n", []string{"SET", "k", "a\nb"}},
		{`SET k "\x41\x42"` + "\r\n", []string{"SET", "k", "AB"}},
		{"SET k 'single quoted'\r\n", []string{"SET", "k", "single quoted"}},
	}
	for _, c := range cases {
		got := readAll(t, c.in)
		if len(got) != 1 || !equalRow(got[0], c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestProtocolErrors(t *testing.T) {
	cases := []string{
		"*1\r\n+PING\r\n",    // element is not a bulk string
		"*3000000000\r\n",    // multibulk count out of range
		"$4\r\n",             // bulk header where a command is expected -> inline, fine
		"*1\r\n$-5\r\nx\r\n", // negative bulk length
		"PING \"unbalanced\r\n",
	}
	for _, c := range cases[:2] {
		rd := NewReader(strings.NewReader(c), DefaultLimits())
		if _, err := rd.ReadCommand(); err == nil {
			t.Errorf("%q: expected protocol error", c)
		} else if _, ok := err.(*ProtocolError); !ok && err != io.EOF && err != io.ErrNoProgress {
			t.Errorf("%q: got %v", c, err)
		}
	}
	for _, c := range cases[3:] {
		rd := NewReader(strings.NewReader(c), DefaultLimits())
		if _, err := rd.ReadCommand(); err == nil {
			t.Errorf("%q: expected protocol error", c)
		}
	}
}

func TestBulkLimitEnforced(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxBulk = 16
	rd := NewReader(strings.NewReader("*1\r\n$100\r\n"), lim)
	if _, err := rd.ReadCommand(); err == nil {
		t.Fatal("expected bulk limit error")
	}
}

func TestQueryBufferLimit(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxQueryBuffer = 32 * 1024
	// A bulk header promising more than the query buffer can hold.
	big := strings.Repeat("x", 40*1024)
	rd := NewReader(strings.NewReader("*2\r\n$3\r\nSET\r\n$40960\r\n"+big), lim)
	if _, err := rd.ReadCommand(); err != ErrQueryBufferLimit {
		t.Fatalf("got %v want ErrQueryBufferLimit", err)
	}
}

func TestReadCommandDoesNotAllocate(t *testing.T) {
	input := strings.Repeat("*2\r\n$3\r\nGET\r\n$5\r\nmykey\r\n", 1000)
	rd := NewReader(strings.NewReader(input), DefaultLimits())
	if _, err := rd.ReadCommand(); err != nil { // warm up buffers
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(500, func() {
		if _, err := rd.ReadCommand(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 0 {
		t.Fatalf("ReadCommand allocated %v times per call, want 0", allocs)
	}
}

func encode(t *testing.T, proto int, v Value) string {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetProtocol(proto)
	w.WriteValue(v)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestWriteRESP2(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{OK(), "+OK\r\n"},
		{Err("ERR boom"), "-ERR boom\r\n"},
		{Int(-12), ":-12\r\n"},
		{BulkString("hey"), "$3\r\nhey\r\n"},
		{BulkString(""), "$0\r\n\r\n"},
		{Null(), "$-1\r\n"},
		{NullArray(), "*-1\r\n"},
		{EmptyArray(), "*0\r\n"},
		{Array(Int(1), BulkString("x")), "*2\r\n:1\r\n$1\r\nx\r\n"},
		{Map([]Value{BulkString("a"), Int(1)}), "*2\r\n$1\r\na\r\n:1\r\n"},
		{Set([]Value{BulkString("a")}), "*1\r\n$1\r\na\r\n"},
		{Double(3.5), "$3\r\n3.5\r\n"},
		{Double(4), "$1\r\n4\r\n"},
		{Bool(true), ":1\r\n"},
		{Bool(false), ":0\r\n"},
		{Verbatim("txt", "hi"), "$2\r\nhi\r\n"},
	}
	for _, c := range cases {
		if got := encode(t, RESP2, c.v); got != c.want {
			t.Errorf("kind %d: got %q want %q", c.v.Kind, got, c.want)
		}
	}
}

func TestWriteRESP3(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Null(), "_\r\n"},
		{NullArray(), "_\r\n"},
		{Map([]Value{BulkString("a"), Int(1)}), "%1\r\n$1\r\na\r\n:1\r\n"},
		{Set([]Value{BulkString("a")}), "~1\r\n$1\r\na\r\n"},
		{Double(3.5), ",3.5\r\n"},
		{Double(inf(1)), ",inf\r\n"},
		{Bool(true), "#t\r\n"},
		{Push(BulkString("message")), ">1\r\n$7\r\nmessage\r\n"},
		{Verbatim("txt", "hi"), "=6\r\ntxt:hi\r\n"},
	}
	for _, c := range cases {
		if got := encode(t, RESP3, c.v); got != c.want {
			t.Errorf("kind %d: got %q want %q", c.v.Kind, got, c.want)
		}
	}
}

func TestWriterBatchesIntoOneWrite(t *testing.T) {
	var c countingWriter
	w := NewWriter(&c)
	for i := 0; i < 100; i++ {
		w.WriteValue(OK())
	}
	if c.calls != 0 {
		t.Fatalf("wrote before flush: %d calls", c.calls)
	}
	w.Flush()
	if c.calls != 1 {
		t.Fatalf("got %d write syscalls, want 1", c.calls)
	}
}

type countingWriter struct{ calls, bytes int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	c.bytes += len(p)
	return len(p), nil
}

func TestRoundTripThroughReplyReader(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetProtocol(RESP3)
	w.WriteValue(Array(BulkString("a"), Int(2), Null(), Double(1.5)))
	w.Flush()
	v, err := NewReplyReader(&buf).ReadReply()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindArray || len(v.Elems) != 4 {
		t.Fatalf("got %+v", v)
	}
	if string(v.Elems[0].Str) != "a" || v.Elems[1].Int != 2 ||
		v.Elems[2].Kind != KindNull || v.Elems[3].Float != 1.5 {
		t.Fatalf("round trip mismatch: %+v", v)
	}
}

func TestEncodeCommand(t *testing.T) {
	got := string(EncodeCommand(nil, []byte("SET"), []byte("k"), []byte("v")))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseInt(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true}, {"-1", -1, true}, {"12345", 12345, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"9223372036854775808", 0, false},
		{"", 0, false}, {"1x", 0, false}, {"-", 0, false}, {" 1", 0, false},
	} {
		got, err := ParseInt([]byte(c.in))
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseInt(%q) = %d, %v", c.in, got, err)
		}
	}
}

func equal(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalRow(a[i], b[i]) {
			return false
		}
	}
	return true
}

func equalRow(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
