package resp

import "math"

func inf(sign int) float64 { return math.Inf(sign) }

// EncodeCommand renders args as a RESP2 command array. It is used by the
// client, the replication link, and the append-only log.
func EncodeCommand(dst []byte, args ...[]byte) []byte {
	dst = append(dst, '*')
	dst = appendInt(dst, int64(len(args)))
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = append(dst, '$')
		dst = appendInt(dst, int64(len(a)))
		dst = append(dst, '\r', '\n')
		dst = append(dst, a...)
		dst = append(dst, '\r', '\n')
	}
	return dst
}

func appendInt(dst []byte, n int64) []byte {
	if n >= 0 && n < 10 {
		return append(dst, byte('0'+n))
	}
	var tmp [20]byte
	i := len(tmp)
	neg := n < 0
	u := uint64(n)
	if neg {
		u = uint64(-n)
	}
	for u >= 10 {
		i--
		tmp[i] = byte('0' + u%10)
		u /= 10
	}
	i--
	tmp[i] = byte('0' + u)
	if neg {
		i--
		tmp[i] = '-'
	}
	return append(dst, tmp[i:]...)
}
