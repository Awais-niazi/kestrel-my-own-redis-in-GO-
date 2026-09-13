package resp

import (
	"errors"
	"io"
	"strconv"
)

// Limits bounds everything a client can make the server allocate (FR-1.6).
type Limits struct {
	MaxBulk        int // largest single bulk argument
	MaxMultiBulk   int // most elements in one command
	MaxInline      int // longest inline command line
	MaxQueryBuffer int // largest pending unparsed input per client
}

// DefaultLimits matches the reference implementation's defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxBulk:        512 * 1024 * 1024,
		MaxMultiBulk:   1024 * 1024,
		MaxInline:      64 * 1024,
		MaxQueryBuffer: 1024 * 1024 * 1024,
	}
}

// ProtocolError is a malformed-input error. The server replies with it and
// then closes the connection (FR-1.7).
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "Protocol error: " + e.Msg }

func protoErr(msg string) error { return &ProtocolError{Msg: msg} }

// errIncomplete signals that the buffer holds a prefix of a command and more
// bytes are needed. It never escapes this package.
var errIncomplete = errors.New("resp: incomplete")

// ErrQueryBufferLimit is returned when a client's unparsed input exceeds
// MaxQueryBuffer.
var ErrQueryBufferLimit = errors.New("resp: query buffer limit exceeded")

const initialReadBuffer = 16 * 1024

// Reader decodes commands from a connection.
//
// It owns its buffer so that decoded arguments are sub-slices of that buffer
// rather than copies: reading a command allocates nothing (PRD §11). The
// arguments returned by ReadCommand are only valid until the next call, so a
// caller that retains an argument beyond the current command must copy it.
//
// A Reader is owned by a single connection goroutine.
type Reader struct {
	src  io.Reader
	buf  []byte
	r, w int
	lim  Limits

	args    [][]byte // reused between commands
	scratch []byte   // unescaping space for quoted inline arguments
}

// NewReader returns a Reader over src with the given limits.
func NewReader(src io.Reader, lim Limits) *Reader {
	if lim.MaxBulk == 0 {
		lim = DefaultLimits()
	}
	return &Reader{
		src:  src,
		buf:  make([]byte, initialReadBuffer),
		lim:  lim,
		args: make([][]byte, 0, 8),
	}
}

// Reset points the Reader at a new source and drops buffered input.
func (rd *Reader) Reset(src io.Reader) {
	rd.src = src
	rd.r, rd.w = 0, 0
}

// Buffered reports whether unparsed bytes remain. A connection handler uses
// this to decide whether to flush its writer or keep draining a pipeline.
func (rd *Reader) Buffered() bool { return rd.r < rd.w }

// ReadCommand returns the next command as a slice of arguments.
//
// Empty commands (a bare newline, or "*0") are skipped transparently. The
// returned slice and its contents alias the Reader's buffer and are invalid
// after the next call.
func (rd *Reader) ReadCommand() ([][]byte, error) {
	for {
		if rd.r < rd.w {
			args, n, err := rd.parse(rd.buf[rd.r:rd.w])
			switch {
			case err == nil:
				rd.r += n
				if len(args) == 0 {
					continue // empty command; keep reading
				}
				return args, nil
			case err != errIncomplete:
				return nil, err
			}
		}
		if err := rd.fill(); err != nil {
			return nil, err
		}
	}
}

// fill makes room in the buffer and reads at least one more byte.
func (rd *Reader) fill() error {
	switch {
	case rd.r == rd.w:
		rd.r, rd.w = 0, 0
	case rd.w == len(rd.buf):
		if rd.r > 0 {
			copy(rd.buf, rd.buf[rd.r:rd.w])
			rd.w -= rd.r
			rd.r = 0
		} else {
			// Buffer is full and entirely occupied by one partial command.
			if len(rd.buf) >= rd.lim.MaxQueryBuffer {
				return ErrQueryBufferLimit
			}
			size := len(rd.buf) * 2
			if size > rd.lim.MaxQueryBuffer {
				size = rd.lim.MaxQueryBuffer
			}
			grown := make([]byte, size)
			copy(grown, rd.buf[:rd.w])
			rd.buf = grown
		}
	}
	n, err := rd.src.Read(rd.buf[rd.w:])
	rd.w += n
	if n > 0 {
		return nil
	}
	if err == nil {
		err = io.ErrNoProgress
	}
	return err
}

// parse decodes one command from b, returning the arguments and how many
// bytes were consumed. It is pure with respect to the buffer: on
// errIncomplete nothing is consumed and the caller may safely compact.
func (rd *Reader) parse(b []byte) ([][]byte, int, error) {
	if b[0] != '*' {
		return rd.parseInline(b)
	}
	return rd.parseMultiBulk(b)
}

func (rd *Reader) parseMultiBulk(b []byte) ([][]byte, int, error) {
	line, pos, err := readLine(b, 0, rd.lim.MaxInline, "invalid multibulk length")
	if err != nil {
		return nil, 0, err
	}
	count, err := parseInt(line[1:])
	if err != nil {
		return nil, 0, protoErr("invalid multibulk length")
	}
	if count <= 0 {
		if count < -1 {
			return nil, 0, protoErr("invalid multibulk length")
		}
		return nil, pos, nil // *0 and *-1 are no-ops
	}
	if count > int64(rd.lim.MaxMultiBulk) {
		return nil, 0, protoErr("invalid multibulk length")
	}

	args := rd.args[:0]
	for i := int64(0); i < count; i++ {
		if pos >= len(b) {
			return nil, 0, errIncomplete
		}
		if b[pos] != '$' {
			return nil, 0, protoErr("expected '$', got '" + string(b[pos:pos+1]) + "'")
		}
		var hdr []byte
		hdr, pos, err = readLine(b, pos, 64, "invalid bulk length")
		if err != nil {
			return nil, 0, err
		}
		n, err := parseInt(hdr[1:])
		if err != nil || n < 0 || n > int64(rd.lim.MaxBulk) {
			return nil, 0, protoErr("invalid bulk length")
		}
		end := pos + int(n)
		if end+2 > len(b) {
			return nil, 0, errIncomplete
		}
		if b[end] != '\r' || b[end+1] != '\n' {
			return nil, 0, protoErr("bad bulk string terminator")
		}
		args = append(args, b[pos:end])
		pos = end + 2
	}
	rd.args = args
	return args, pos, nil
}

// readLine returns the CRLF- or LF-terminated line starting at pos (excluding
// the terminator) and the offset just past it.
func readLine(b []byte, pos, max int, what string) ([]byte, int, error) {
	for i := pos; i < len(b); i++ {
		if b[i] == '\n' {
			end := i
			if end > pos && b[end-1] == '\r' {
				end--
			}
			return b[pos:end], i + 1, nil
		}
	}
	if len(b)-pos > max {
		return nil, 0, protoErr(what)
	}
	return nil, 0, errIncomplete
}

// parseInt parses a decimal integer without allocating.
func parseInt(b []byte) (int64, error) {
	if len(b) == 0 || len(b) > 20 {
		return 0, strconv.ErrSyntax
	}
	neg := false
	i := 0
	if b[0] == '-' || b[0] == '+' {
		neg = b[0] == '-'
		i++
		if i == len(b) {
			return 0, strconv.ErrSyntax
		}
	}
	var n int64
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
		d := int64(c - '0')
		if n > (1<<63-1-d)/10 {
			return 0, strconv.ErrRange
		}
		n = n*10 + d
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ParseInt exposes the allocation-free integer parser to the command layer.
func ParseInt(b []byte) (int64, error) { return parseInt(b) }
