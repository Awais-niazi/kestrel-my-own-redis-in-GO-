package persist

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// Record is one decoded effect.
//
// Args alias the reader's buffer and are valid only until the next call to
// Next, the same contract the protocol reader uses. Recovery hands them
// straight to the dispatcher and never retains them; anything that does must
// copy.
type Record struct {
	DB     int
	Args   [][]byte
	Offset uint64 // stream offset of this record
	Size   int    // encoded size, header included
}

// Reader iterates the records of a log file.
type Reader struct {
	src  *bufio.Reader
	base uint64
	off  uint64 // stream offset of the next record to read
	hdr  [recordHeaderSize]byte
	buf  []byte
	rec  Record
	err  error
}

// NewReader validates a log file header and returns a reader positioned at
// the first record.
func NewReader(src io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(src, 64<<10)
	var hb [fileHeaderSize]byte
	if _, err := io.ReadFull(br, hb[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: file is shorter than its header", ErrBadHeader)
		}
		return nil, err
	}
	h, err := decodeFileHeader(hb[:])
	if err != nil {
		return nil, err
	}
	return &Reader{src: br, base: h.Base, off: h.Base, buf: make([]byte, 0, 4096)}, nil
}

// OpenReader opens a log file for reading. The caller closes the returned
// file.
func OpenReader(path string) (*Reader, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r, err := NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("persist: %s: %w", path, err)
	}
	return r, f, nil
}

// Next decodes the next record. It returns false at the end of the log and
// on the first record that cannot be read, which Err then describes.
func (r *Reader) Next() bool {
	if r.err != nil {
		return false
	}
	n, err := io.ReadFull(r.src, r.hdr[:])
	switch {
	case errors.Is(err, io.EOF) && n == 0:
		return false // clean end of log
	case err != nil:
		r.err = fmt.Errorf("%w: header at offset %d is %d of %d bytes",
			ErrTornTail, r.off, n, recordHeaderSize)
		return false
	}

	size := int(binary.LittleEndian.Uint32(r.hdr[0:]))
	db := int(binary.LittleEndian.Uint16(r.hdr[4:]))
	want := binary.LittleEndian.Uint32(r.hdr[8:])

	// The length is checked before it is used to allocate. A record header
	// whose checksum is wrong can name any size at all, and the checksum
	// cannot be verified until the payload has been read.
	if size < 0 || size > MaxRecordSize {
		r.err = fmt.Errorf("%w: record at offset %d claims %d bytes",
			ErrCorrupt, r.off, size)
		return false
	}

	if cap(r.buf) < size {
		r.buf = make([]byte, size)
	}
	p := r.buf[:size]
	if n, err := io.ReadFull(r.src, p); err != nil {
		r.err = fmt.Errorf("%w: payload at offset %d is %d of %d bytes",
			ErrTornTail, r.off, n, size)
		return false
	}

	sum := crc32.Update(crc32.Checksum(r.hdr[:8], crcTable), crcTable, p)
	if sum != want {
		r.err = fmt.Errorf("%w: checksum mismatch at offset %d", ErrCorrupt, r.off)
		return false
	}

	args, err := decodeRESPArray(p)
	if err != nil {
		r.err = fmt.Errorf("%s at offset %d", err, r.off)
		return false
	}
	if len(args) == 0 {
		r.err = fmt.Errorf("%w: empty command at offset %d", ErrCorrupt, r.off)
		return false
	}

	r.rec = Record{DB: db, Args: args, Offset: r.off, Size: recordHeaderSize + size}
	r.off += uint64(r.rec.Size)
	return true
}

// Record returns the record most recently decoded by Next.
func (r *Reader) Record() Record { return r.rec }

// Offset is the stream offset just past the last record read successfully.
// Recovery truncates a damaged log to this point.
func (r *Reader) Offset() uint64 { return r.off }

// Err reports why iteration stopped. It is nil at a clean end of log, and
// wraps ErrTornTail or ErrCorrupt otherwise.
//
// The two are kept apart because they mean different things to an operator:
// a torn tail is the ordinary result of a crash during a write and costs at
// most the last command, while a checksum failure in the middle of a file is
// storage damage, and truncating at that point silently discards every write
// that followed it.
func (r *Reader) Err() error { return r.err }
