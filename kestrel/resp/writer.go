package resp

import (
	"io"
	"math"
	"strconv"
)

// Protocol versions negotiated via HELLO.
const (
	RESP2 = 2
	RESP3 = 3
)

// DefaultWriteBuffer is the initial size of a Writer's output buffer.
const DefaultWriteBuffer = 16 * 1024

// Writer serializes reply trees onto an io.Writer.
//
// Output accumulates in an internal buffer and is only handed to the
// underlying writer on Flush, so a pipeline of N commands produces one
// write(2) (see PRD §11, "write batching").
//
// A Writer is owned by a single connection goroutine and is not safe for
// concurrent use.
type Writer struct {
	w     io.Writer
	buf   []byte
	proto int
	err   error

	// MaxBuffer, when non-zero, caps how large the pending buffer may grow
	// before Write forces an intermediate flush. This keeps a client that
	// pipelines millions of commands from ballooning the heap (NFR-4).
	MaxBuffer int
}

// NewWriter returns a Writer speaking RESP2 by default.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, DefaultWriteBuffer), proto: RESP2}
}

// Protocol reports the negotiated protocol version.
func (w *Writer) Protocol() int { return w.proto }

// SetProtocol switches the serialization dialect. Called by HELLO.
func (w *Writer) SetProtocol(v int) { w.proto = v }

// Buffered reports how many bytes are pending.
func (w *Writer) Buffered() int { return len(w.buf) }

// Err reports the first write error seen, if any. Once set, the Writer is
// inert: further calls are no-ops.
func (w *Writer) Err() error { return w.err }

// Reset points the Writer at a new destination and clears pending state.
func (w *Writer) Reset(dst io.Writer) {
	w.w = dst
	w.buf = w.buf[:0]
	w.err = nil
	w.proto = RESP2
}

// Flush writes pending bytes to the underlying writer.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.w.Write(w.buf)
	w.buf = w.buf[:0]
	if err != nil {
		w.err = err
	}
	return err
}

// WriteValue serializes v according to the negotiated protocol.
func (w *Writer) WriteValue(v Value) {
	if w.err != nil {
		return
	}
	w.encode(v)
	if w.MaxBuffer > 0 && len(w.buf) >= w.MaxBuffer {
		w.Flush()
	}
}

// WriteRaw appends already-encoded protocol bytes. Used by MONITOR, the
// replication link, and inline protocol errors.
func (w *Writer) WriteRaw(b []byte) {
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, b...)
}

func (w *Writer) encode(v Value) {
	switch v.Kind {
	case KindNone:
		return
	case KindSimple:
		w.line('+', v.Str)
	case KindError:
		w.line('-', v.Str)
	case KindInt:
		w.buf = append(w.buf, ':')
		w.buf = strconv.AppendInt(w.buf, v.Int, 10)
		w.crlf()
	case KindBulk:
		w.bulk(v.Str)
	case KindArray:
		w.aggregate('*', v.Elems)
	case KindNull:
		if w.proto == RESP3 {
			w.buf = append(w.buf, '_', '\r', '\n')
		} else {
			w.buf = append(w.buf, '$', '-', '1', '\r', '\n')
		}
	case KindNullArray:
		if w.proto == RESP3 {
			w.buf = append(w.buf, '_', '\r', '\n')
		} else {
			w.buf = append(w.buf, '*', '-', '1', '\r', '\n')
		}
	case KindMap:
		if w.proto == RESP3 {
			w.header('%', len(v.Elems)/2)
			for i := range v.Elems {
				w.encode(v.Elems[i])
			}
		} else {
			w.aggregate('*', v.Elems)
		}
	case KindSet:
		if w.proto == RESP3 {
			w.aggregate('~', v.Elems)
		} else {
			w.aggregate('*', v.Elems)
		}
	case KindPush:
		if w.proto == RESP3 {
			w.aggregate('>', v.Elems)
		} else {
			w.aggregate('*', v.Elems)
		}
	case KindDouble:
		if w.proto == RESP3 {
			w.buf = append(w.buf, ',')
			w.buf = appendDouble(w.buf, v.Float)
			w.crlf()
		} else {
			var tmp [40]byte
			w.bulk(appendDouble(tmp[:0], v.Float))
		}
	case KindBool:
		if w.proto == RESP3 {
			if v.Bool {
				w.buf = append(w.buf, '#', 't', '\r', '\n')
			} else {
				w.buf = append(w.buf, '#', 'f', '\r', '\n')
			}
		} else {
			if v.Bool {
				w.buf = append(w.buf, ':', '1', '\r', '\n')
			} else {
				w.buf = append(w.buf, ':', '0', '\r', '\n')
			}
		}
	case KindBigNum:
		if w.proto == RESP3 {
			w.line('(', v.Str)
		} else {
			w.bulk(v.Str)
		}
	case KindVerbatim:
		if w.proto == RESP3 {
			f := v.Fmt
			if len(f) != 3 {
				f = "txt"
			}
			w.buf = append(w.buf, '=')
			w.buf = strconv.AppendInt(w.buf, int64(len(v.Str)+4), 10)
			w.crlf()
			w.buf = append(w.buf, f...)
			w.buf = append(w.buf, ':')
			w.buf = append(w.buf, v.Str...)
			w.crlf()
		} else {
			w.bulk(v.Str)
		}
	default:
		w.line('-', []byte("ERR internal: unknown reply kind"))
	}
}

func (w *Writer) line(prefix byte, s []byte) {
	w.buf = append(w.buf, prefix)
	w.buf = append(w.buf, s...)
	w.crlf()
}

func (w *Writer) bulk(s []byte) {
	w.buf = append(w.buf, '$')
	w.buf = strconv.AppendInt(w.buf, int64(len(s)), 10)
	w.crlf()
	w.buf = append(w.buf, s...)
	w.crlf()
}

// WriteArrayHeader opens an array of n elements without writing them.
//
// It exists for MULTI/EXEC, where each queued command writes its own reply
// into the array -- including the handful that write their output directly
// rather than returning a value. Collecting those into a slice first would
// mean giving them a second way to produce a reply.
//
// The caller must then write exactly n values.
func (w *Writer) WriteArrayHeader(n int) { w.header('*', n) }

func (w *Writer) header(prefix byte, n int) {
	w.buf = append(w.buf, prefix)
	w.buf = strconv.AppendInt(w.buf, int64(n), 10)
	w.crlf()
}

func (w *Writer) aggregate(prefix byte, elems []Value) {
	w.header(prefix, len(elems))
	for i := range elems {
		w.encode(elems[i])
	}
}

func (w *Writer) crlf() { w.buf = append(w.buf, '\r', '\n') }

// appendDouble renders a float the way the reference implementation does:
// infinities are spelled out and finite values use the shortest round-trip
// representation.
func appendDouble(dst []byte, f float64) []byte {
	switch {
	case math.IsInf(f, 1):
		return append(dst, "inf"...)
	case math.IsInf(f, -1):
		return append(dst, "-inf"...)
	case math.IsNaN(f):
		return append(dst, "nan"...)
	case f == math.Trunc(f) && math.Abs(f) < 1e17:
		// Whole numbers are rendered without a fractional part.
		return strconv.AppendInt(dst, int64(f), 10)
	default:
		return strconv.AppendFloat(dst, f, 'g', 17, 64)
	}
}

// FormatDouble renders f using the wire representation for doubles.
func FormatDouble(f float64) string { return string(appendDouble(nil, f)) }
