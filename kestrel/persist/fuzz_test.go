package persist

import (
	"bytes"
	"testing"
)

// FuzzReader feeds arbitrary bytes to the reader as if they were a log file.
//
// The property is not that anything decodes -- almost nothing will -- but
// that a damaged or hostile file is rejected rather than crashing the
// process or driving a huge allocation. Recovery runs before the server
// accepts a connection, so a panic here is a server that cannot start.
func FuzzReader(f *testing.F) {
	var good bytes.Buffer
	good.Write(fileHeader{Version: fileVersion}.encode(logMagic))
	good.Write(encodeRecord(nil, 0, KindEffect, cmd("SET", "a", "1")))
	good.Write(encodeRecord(nil, 9, KindEffect, cmd("DEL", "a")))
	f.Add(good.Bytes())
	f.Add(good.Bytes()[:len(good.Bytes())-3]) // torn tail
	f.Add(fileHeader{Version: fileVersion}.encode(logMagic))
	f.Add([]byte("KESTRLOG"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := NewReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		n := 0
		for r.Next() {
			rec := r.Record()
			if len(rec.Args) == 0 {
				t.Fatal("Next returned a record with no arguments")
			}
			if rec.Size <= recordHeaderSize {
				t.Fatalf("record size %d is not larger than a header", rec.Size)
			}
			if n++; n > 1000 {
				t.Fatal("reader did not terminate")
			}
		}
		if r.Offset() > uint64(len(data)) {
			t.Fatalf("reader ran past the end of the input: %d > %d", r.Offset(), len(data))
		}
	})
}

// FuzzRecordRoundTrip checks that whatever a command produces, the log can
// carry it back unchanged: empty arguments, embedded newlines and NULs, and
// binary values are all things a real key or value contains.
func FuzzRecordRoundTrip(f *testing.F) {
	f.Add("SET", "key", "value")
	f.Add("SET", "", "")
	f.Add("RPUSH", "k\r\n", "\x00\xff")
	f.Add("*1\r\n", "$3\r\n", "\n")

	f.Fuzz(func(t *testing.T, a, b, c string) {
		args := [][]byte{[]byte(a), []byte(b), []byte(c)}
		enc := encodeRecord(nil, 7, KindEffect, args)
		if got := recordSize(args); got != len(enc) {
			t.Fatalf("recordSize said %d, encoding took %d", got, len(enc))
		}

		var buf bytes.Buffer
		buf.Write(fileHeader{Version: fileVersion}.encode(logMagic))
		buf.Write(enc)

		r, err := NewReader(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Next() {
			t.Fatalf("record did not decode: %v", r.Err())
		}
		rec := r.Record()
		if rec.DB != 7 {
			t.Errorf("db is %d, want 7", rec.DB)
		}
		if len(rec.Args) != 3 {
			t.Fatalf("decoded %d arguments, want 3", len(rec.Args))
		}
		for i, want := range args {
			if !bytes.Equal(rec.Args[i], want) {
				t.Errorf("argument %d: got %q, want %q", i, rec.Args[i], want)
			}
		}
		if r.Next() {
			t.Error("a second record appeared from nowhere")
		}
		if r.Err() != nil {
			t.Errorf("clean log reported %v", r.Err())
		}
	})
}
