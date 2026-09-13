package resp

import (
	"bytes"
	"testing"
)

// FuzzReadCommand drives arbitrary bytes through the command parser.
//
// The parser is one of the three places untrusted bytes enter the process
// (§12), and it must never panic, never loop forever, and never report
// success while pointing at memory outside the buffer it was given.
func FuzzReadCommand(f *testing.F) {
	seeds := []string{
		"*1\r\n$4\r\nPING\r\n",
		"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
		"PING\r\n",
		"SET k \"quoted value\"\r\n",
		"*-1\r\n",
		"*0\r\n",
		"*2\r\n$3\r\nGET\r\n$-1\r\n",
		"$5\r\nhello\r\n",
		"*99999999999999999999\r\n",
		"*1\r\n$99999999999999999999\r\n",
		"\x00\x01\x02\r\n",
		"*2\r\n$1\r\na\r\n$1\r\n",
		"SET k 'unbalanced\r\n",
		"*1\r\n$4\r\nPING\r",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		lim := Limits{
			MaxBulk:        1 << 16,
			MaxMultiBulk:   1024,
			MaxInline:      1024,
			MaxQueryBuffer: 1 << 18,
		}
		rd := NewReader(bytes.NewReader(data), lim)
		// A bounded number of commands: a parser bug that consumed nothing
		// per call would otherwise show up as a timeout rather than a
		// failure.
		for i := 0; i < len(data)+16; i++ {
			args, err := rd.ReadCommand()
			if err != nil {
				return
			}
			if len(args) == 0 {
				t.Fatal("ReadCommand returned no arguments and no error")
			}
			for _, a := range args {
				if len(a) > lim.MaxBulk {
					t.Fatalf("argument of %d bytes exceeds the configured limit", len(a))
				}
			}
		}
		t.Fatal("parser produced more commands than the input could contain")
	})
}

// FuzzWriteValue checks that anything the writer emits can be read back.
func FuzzWriteValue(f *testing.F) {
	f.Add("hello", int64(42), true)
	f.Add("", int64(0), false)
	f.Add("with\r\nnewlines", int64(-1), true)

	f.Fuzz(func(t *testing.T, s string, n int64, flag bool) {
		for _, proto := range []int{RESP2, RESP3} {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			w.SetProtocol(proto)
			w.WriteValue(Array(Bulk([]byte(s)), Int(n), Bool(flag), Null()))
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			v, err := NewReplyReader(&buf).ReadReply()
			if err != nil {
				t.Fatalf("proto %d: %v", proto, err)
			}
			if len(v.Elems) != 4 {
				t.Fatalf("proto %d: got %d elements", proto, len(v.Elems))
			}
			if string(v.Elems[0].Str) != s {
				t.Fatalf("proto %d: bulk round trip changed %q to %q", proto, s, v.Elems[0].Str)
			}
			if v.Elems[1].Int != n {
				t.Fatalf("proto %d: integer round trip changed %d to %d", proto, n, v.Elems[1].Int)
			}
		}
	})
}
