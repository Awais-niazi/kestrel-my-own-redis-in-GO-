// Package persist implements the append log, the snapshotter and recovery
// (M3).
//
// The log stores the canonical effect of every write, in order, as produced
// by the propagation path the command layer has exercised since M1 (ADR-008).
// It is logical, not physical: a record holds the arguments of a replay-safe
// command, so the log grows with the number of writes rather than with the
// size of the values they touch.
//
// The package depends on nothing but the standard library. In particular it
// does not use the resp package, even though a record payload is a RESP2
// array. A connection's writer switches dialect when a client sends HELLO 3,
// and a log whose encoding could follow a client's protocol negotiation
// would be a file whose meaning depends on who happened to be connected.
// The twenty lines of encoder here are fixed at RESP2 for good.
package persist

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
)

// Log file layout.
//
// A file begins with a 32-byte header and is followed by records until the
// end of the file:
//
//	header  magic[8] | version:u16 | flags:u16 | base:u64 | created:u64 | crc:u32
//	record  len:u32 | db:u16 | flags:u16 | crc:u32 | payload[len]
//
// base is the stream offset of this file's first record. Stream offsets run
// across files, so a rewrite that starts a new file carries the counter
// forward rather than restarting it: an offset recorded by a snapshot stays
// meaningful after a compaction.
//
// The record checksum covers the first eight header bytes as well as the
// payload, so a corrupted length is caught rather than acted on. It still
// has to be read before it can be checked, which is why it is bounded by
// MaxRecordSize and by the bytes remaining in the file.
const (
	// logMagic and snapshotMagic distinguish the two file kinds. They share
	// a framing, a checksum and a decoder, and differ in what the records
	// mean, so opening one as the other has to fail rather than half work.
	logMagic         = "KESTRLOG"
	snapshotMagic    = "KESTRSNP"
	fileVersion      = 1
	fileHeaderSize   = 32
	recordHeaderSize = 12

	// MaxRecordSize bounds the payload of a single record. A record larger
	// than this is treated as corruption rather than allocated for.
	MaxRecordSize = 512 << 20
)

// RecordKind distinguishes the payloads a file can hold. It travels in the
// record header's flags field.
//
// Keeping anchors in the same stream as effects means one framing, one
// checksum and one decoder for both. Keeping them in a separate *kind* means
// replay can never be talked into executing one as a command, however well
// formed it looks.
type RecordKind uint16

// Record kinds.
const (
	// KindEffect is a command to be replayed.
	KindEffect RecordKind = iota
	// KindAnchor records the log offset a shard was serialized at. It
	// appears only in a snapshot.
	KindAnchor
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// zeroHeader reserves space for a record header that is filled in once the
// payload length is known, without allocating a slice per record.
var zeroHeader [recordHeaderSize]byte

// ErrCorrupt reports a record that cannot be trusted: a bad checksum, an
// impossible length, or a payload that is not a RESP2 array of bulk strings.
// It is distinct from a torn tail, because the two have different causes and
// the operator has to be told which one happened.
var ErrCorrupt = errors.New("persist: corrupt log record")

// ErrTornTail reports a record that stops before it ends. This is the
// expected result of a crash part-way through a write, not damage, and
// recovery truncates it.
var ErrTornTail = errors.New("persist: torn record at end of log")

// ErrBadHeader reports a file that is not a kestrel log, or is one written
// by an incompatible version.
var ErrBadHeader = errors.New("persist: not a kestrel log file")

// fileHeader is the fixed prologue of a log file.
type fileHeader struct {
	Version uint16
	Flags   uint16
	Base    uint64 // stream offset of the first record in this file
	Created uint64 // unix milliseconds
}

func (h fileHeader) encode(magic string) []byte {
	b := make([]byte, fileHeaderSize)
	copy(b, magic)
	binary.LittleEndian.PutUint16(b[8:], h.Version)
	binary.LittleEndian.PutUint16(b[10:], h.Flags)
	binary.LittleEndian.PutUint64(b[12:], h.Base)
	binary.LittleEndian.PutUint64(b[20:], h.Created)
	binary.LittleEndian.PutUint32(b[28:], crc32.Checksum(b[:28], crcTable))
	return b
}

func decodeFileHeader(b []byte, magic string) (fileHeader, error) {
	var h fileHeader
	if len(b) < fileHeaderSize {
		return h, ErrBadHeader
	}
	if got := string(b[:8]); got != magic {
		return h, fmt.Errorf("%w: header says %q, expected %q", ErrBadHeader, got, magic)
	}
	if got := binary.LittleEndian.Uint32(b[28:]); got != crc32.Checksum(b[:28], crcTable) {
		return h, fmt.Errorf("%w: header checksum mismatch", ErrCorrupt)
	}
	h.Version = binary.LittleEndian.Uint16(b[8:])
	h.Flags = binary.LittleEndian.Uint16(b[10:])
	h.Base = binary.LittleEndian.Uint64(b[12:])
	h.Created = binary.LittleEndian.Uint64(b[20:])
	if h.Version != fileVersion {
		return h, fmt.Errorf("%w: version %d, want %d", ErrBadHeader, h.Version, fileVersion)
	}
	return h, nil
}

// encodeRecord appends one framed record for args to dst and returns the
// extended slice. The caller reuses dst across appends, so a write costs no
// allocation once the buffer has grown.
func encodeRecord(dst []byte, db int, kind RecordKind, args [][]byte) []byte {
	start := len(dst)
	dst = append(dst, zeroHeader[:]...)
	payloadStart := len(dst)
	dst = appendRESPArray(dst, args)
	n := len(dst) - payloadStart

	h := dst[start : start+recordHeaderSize]
	binary.LittleEndian.PutUint32(h[0:], uint32(n))
	binary.LittleEndian.PutUint16(h[4:], uint16(db))
	binary.LittleEndian.PutUint16(h[6:], uint16(kind))
	sum := crc32.Update(crc32.Checksum(h[:8], crcTable), crcTable, dst[payloadStart:])
	binary.LittleEndian.PutUint32(h[8:], sum)
	return dst
}

// recordSize is the encoded size of a record, used to size a buffer exactly.
func recordSize(args [][]byte) int {
	n := recordHeaderSize + 1 + digits(len(args)) + 2
	for _, a := range args {
		n += 1 + digits(len(a)) + 2 + len(a) + 2
	}
	return n
}

func digits(n int) int {
	if n < 10 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

// appendRESPArray writes args as a RESP2 array of bulk strings. The dialect
// is fixed: see the package comment.
func appendRESPArray(dst []byte, args [][]byte) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(a)), 10)
		dst = append(dst, '\r', '\n')
		dst = append(dst, a...)
		dst = append(dst, '\r', '\n')
	}
	return dst
}

// decodeRESPArray parses a payload written by appendRESPArray. It rejects
// anything else, including the RESP3 forms and a nested array, because a
// payload the log did not write is corruption however well formed it looks.
//
// The returned slices alias p, which the reader owns until the next record.
func decodeRESPArray(p []byte) ([][]byte, error) {
	if len(p) == 0 || p[0] != '*' {
		return nil, fmt.Errorf("%w: payload is not an array", ErrCorrupt)
	}
	n, rest, err := readLine(p[1:])
	if err != nil {
		return nil, err
	}
	if n < 0 || n > len(p) {
		return nil, fmt.Errorf("%w: array length %d", ErrCorrupt, n)
	}
	args := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if len(rest) == 0 || rest[0] != '$' {
			return nil, fmt.Errorf("%w: element %d is not a bulk string", ErrCorrupt, i)
		}
		var size int
		size, rest, err = readLine(rest[1:])
		if err != nil {
			return nil, err
		}
		if size < 0 || size+2 > len(rest) {
			return nil, fmt.Errorf("%w: element %d has length %d", ErrCorrupt, i, size)
		}
		if rest[size] != '\r' || rest[size+1] != '\n' {
			return nil, fmt.Errorf("%w: element %d is not terminated", ErrCorrupt, i)
		}
		args = append(args, rest[:size])
		rest = rest[size+2:]
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes in payload", ErrCorrupt, len(rest))
	}
	return args, nil
}

// readLine reads a CRLF-terminated decimal and returns it with the remainder.
func readLine(p []byte) (int, []byte, error) {
	for i := 0; i+1 < len(p); i++ {
		if p[i] != '\r' {
			continue
		}
		if p[i+1] != '\n' {
			return 0, nil, fmt.Errorf("%w: stray carriage return", ErrCorrupt)
		}
		v, err := strconv.Atoi(string(p[:i]))
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %q is not a length", ErrCorrupt, p[:i])
		}
		return v, p[i+2:], nil
	}
	return 0, nil, fmt.Errorf("%w: unterminated length", ErrCorrupt)
}
