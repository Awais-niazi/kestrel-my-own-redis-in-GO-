package resp

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// ReplyReader decodes server replies. It is used by kestrel-cli, by the
// replication follower, and by tests. It is deliberately simple rather than
// allocation-free: none of its callers are on the server hot path.
type ReplyReader struct{ br *bufio.Reader }

// NewReplyReader returns a ReplyReader over r.
func NewReplyReader(r io.Reader) *ReplyReader { return &ReplyReader{br: bufio.NewReader(r)} }

// Buffered reports how many bytes are already buffered.
func (r *ReplyReader) Buffered() int { return r.br.Buffered() }

// ReadReply decodes one reply.
func (r *ReplyReader) ReadReply() (Value, error) {
	prefix, err := r.br.ReadByte()
	if err != nil {
		return Value{}, err
	}
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	switch prefix {
	case '+':
		return Value{Kind: KindSimple, Str: line}, nil
	case '-':
		return Value{Kind: KindError, Str: line}, nil
	case ':':
		n, err := strconv.ParseInt(string(line), 10, 64)
		return Value{Kind: KindInt, Int: n}, err
	case '_':
		return Value{Kind: KindNull}, nil
	case '#':
		return Value{Kind: KindBool, Bool: len(line) > 0 && line[0] == 't'}, nil
	case ',':
		return Value{Kind: KindDouble, Float: parseDouble(string(line))}, nil
	case '(':
		return Value{Kind: KindBigNum, Str: line}, nil
	case '$', '=':
		n, err := strconv.Atoi(string(line))
		if err != nil {
			return Value{}, protoErr("invalid bulk length")
		}
		if n < 0 {
			return Value{Kind: KindNull}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r.br, buf); err != nil {
			return Value{}, err
		}
		v := Value{Kind: KindBulk, Str: buf[:n]}
		if prefix == '=' && n >= 4 {
			v.Kind, v.Fmt, v.Str = KindVerbatim, string(buf[:3]), buf[4:n]
		}
		return v, nil
	case '*', '~', '>', '%':
		n, err := strconv.Atoi(string(line))
		if err != nil {
			return Value{}, protoErr("invalid multibulk length")
		}
		if n < 0 {
			return Value{Kind: KindNullArray}, nil
		}
		if prefix == '%' {
			n *= 2
		}
		elems := make([]Value, n)
		for i := 0; i < n; i++ {
			if elems[i], err = r.ReadReply(); err != nil {
				return Value{}, err
			}
		}
		return Value{Kind: kindForPrefix(prefix), Elems: elems}, nil
	default:
		return Value{}, protoErr("unknown reply type byte '" + string(prefix) + "'")
	}
}

func kindForPrefix(p byte) Kind {
	switch p {
	case '~':
		return KindSet
	case '>':
		return KindPush
	case '%':
		return KindMap
	default:
		return KindArray
	}
}

func (r *ReplyReader) readLine() ([]byte, error) {
	line, err := r.br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	line = line[:len(line)-1]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line, nil
}

func parseDouble(s string) float64 {
	switch strings.ToLower(s) {
	case "inf", "+inf":
		return inf(1)
	case "-inf":
		return inf(-1)
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
