package resp

// Inline commands are space-separated and newline-terminated. They exist so
// that telnet and redis-cli edge cases keep working (FR-1.3); they are not a
// hot path, but they must tokenize exactly like the reference implementation,
// including quoting and escapes.

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func (rd *Reader) parseInline(b []byte) ([][]byte, int, error) {
	line, pos, err := readLine(b, 0, rd.lim.MaxInline, "too big inline request")
	if err != nil {
		return nil, 0, err
	}
	if len(line) == 0 {
		return nil, pos, nil
	}

	// Unescaping never lengthens a token, so reserving len(line) bytes means
	// scratch cannot reallocate mid-parse and the sub-slices we hand back
	// stay valid.
	if cap(rd.scratch) < len(line) {
		rd.scratch = make([]byte, 0, len(line))
	}
	rd.scratch = rd.scratch[:0]

	args := rd.args[:0]
	i := 0
	for {
		for i < len(line) && isSpace(line[i]) {
			i++
		}
		if i >= len(line) {
			break
		}
		var arg []byte
		switch line[i] {
		case '"':
			arg, i, err = rd.readDoubleQuoted(line, i+1)
		case '\'':
			arg, i, err = rd.readSingleQuoted(line, i+1)
		default:
			start := i
			for i < len(line) && !isSpace(line[i]) {
				i++
			}
			arg = line[start:i]
		}
		if err != nil {
			return nil, 0, err
		}
		args = append(args, arg)
	}
	rd.args = args
	return args, pos, nil
}

func (rd *Reader) readDoubleQuoted(line []byte, i int) ([]byte, int, error) {
	start := len(rd.scratch)
	for i < len(line) {
		c := line[i]
		switch {
		case c == '\\' && i+3 < len(line) && line[i+1] == 'x' &&
			isHex(line[i+2]) && isHex(line[i+3]):
			rd.scratch = append(rd.scratch, hexVal(line[i+2])<<4|hexVal(line[i+3]))
			i += 4
		case c == '\\' && i+1 < len(line):
			rd.scratch = append(rd.scratch, unescape(line[i+1]))
			i += 2
		case c == '"':
			i++
			if i < len(line) && !isSpace(line[i]) {
				return nil, 0, protoErr("unbalanced quotes in request")
			}
			end := len(rd.scratch)
			return rd.scratch[start:end:end], i, nil
		default:
			rd.scratch = append(rd.scratch, c)
			i++
		}
	}
	return nil, 0, protoErr("unbalanced quotes in request")
}

func (rd *Reader) readSingleQuoted(line []byte, i int) ([]byte, int, error) {
	start := len(rd.scratch)
	for i < len(line) {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line) && line[i+1] == '\'':
			rd.scratch = append(rd.scratch, '\'')
			i += 2
		case c == '\'':
			i++
			if i < len(line) && !isSpace(line[i]) {
				return nil, 0, protoErr("unbalanced quotes in request")
			}
			end := len(rd.scratch)
			return rd.scratch[start:end:end], i, nil
		default:
			rd.scratch = append(rd.scratch, c)
			i++
		}
	}
	return nil, 0, protoErr("unbalanced quotes in request")
}

func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	case 'a':
		return '\a'
	default:
		return c
	}
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
