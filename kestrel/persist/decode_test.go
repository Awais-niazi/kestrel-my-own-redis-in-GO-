package persist

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFsyncEverySecForcesInBackground covers the policy the server runs by
// default. The append itself must not force, and the background pass must.
func TestFsyncEverySecForcesInBackground(t *testing.T) {
	l := tempLog(t, segmentOptions{Fsync: FsyncEverySec})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	if got := l.Stats().Syncs; got != 0 {
		t.Fatalf("everysec forced %d times during the append itself", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l.Stats().Syncs > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the background pass never forced the log")
}

// TestRecordSizeMatchesEncoding guards the buffer sizing. A recordSize that
// is too small costs an allocation per append and nothing else, so it will
// not show up as a failure anywhere: it has to be checked directly.
func TestRecordSizeMatchesEncoding(t *testing.T) {
	cases := [][][]byte{
		cmd("PING"),
		cmd("SET", "", ""),
		cmd("SET", strings.Repeat("k", 9), strings.Repeat("v", 10)),
		cmd("SET", strings.Repeat("k", 99), strings.Repeat("v", 100)),
		cmd("SET", strings.Repeat("k", 1000), strings.Repeat("v", 100000)),
	}
	many := make([][]byte, 0, 128)
	for i := 0; i < 128; i++ {
		many = append(many, fmt.Append(nil, i))
	}
	cases = append(cases, many)

	for i, args := range cases {
		enc := encodeRecord(nil, 0, KindEffect, args)
		if got := recordSize(args); got != len(enc) {
			t.Errorf("case %d (%d args): recordSize %d, encoded %d", i, len(args), got, len(enc))
		}
	}
}

func TestDigits(t *testing.T) {
	for n, want := range map[int]int{0: 1, 1: 1, 9: 1, 10: 2, 99: 2, 100: 3, 999999: 6} {
		if got := digits(n); got != want {
			t.Errorf("digits(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestDecodeRESPArrayRejects covers the payloads a checksum cannot catch:
// bytes that are intact but were never written by this log. A record can
// only reach the decoder with a valid checksum, so these stand for a log
// written by something else, or by a future version.
func TestDecodeRESPArrayRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"not an array":         "+OK\r\n",
		"unterminated count":   "*2",
		"bad count":            "*x\r\n",
		"negative count":       "*-1\r\n$1\r\na\r\n",
		"count too large":      "*99\r\n",
		"element not bulk":     "*1\r\n+OK\r\n",
		"bad element length":   "*1\r\n$x\r\na\r\n",
		"element too long":     "*1\r\n$99\r\nab\r\n",
		"element unterminated": "*1\r\n$1\r\naXY", // X where the CR belongs
		"element short":        "*1\r\n$1\r\nab",
		"trailing bytes":       "*1\r\n$1\r\na\r\njunk",
		"stray CR":             "*1\r$1\r\na\r\n",
		"negative length":      "*1\r\n$-1\r\n",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRESPArray([]byte(payload)); !errors.Is(err, ErrCorrupt) {
				t.Errorf("decoding %q returned %v, want ErrCorrupt", payload, err)
			}
		})
	}
}

func TestDecodeRESPArrayAcceptsWhatItWrites(t *testing.T) {
	args := cmd("SET", "k", "")
	got, err := decodeRESPArray(appendRESPArray(nil, args))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0]) != "SET" || string(got[2]) != "" {
		t.Errorf("round trip produced %q", got)
	}
}

func TestOpenMissingFile(t *testing.T) {
	_, err := openSegment(segmentOptions{Path: filepath.Join(t.TempDir(), "absent.log")})
	if err == nil {
		t.Fatal("opening a log that does not exist succeeded")
	}
}

func TestCloseIsIdempotentAndSticky(t *testing.T) {
	l := tempLog(t, segmentOptions{})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(0, cmd("SET", "b", "2")); err == nil {
		t.Error("append to a closed log succeeded")
	}
	// Close is called again by the test cleanup; it must not panic on the
	// already-closed stop channel.
}
