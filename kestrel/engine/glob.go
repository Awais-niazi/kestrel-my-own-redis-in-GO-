package engine

// MatchPattern reports whether s matches a glob pattern.
//
// The syntax is the one KEYS, SCAN MATCH and PSUBSCRIBE use: '*' matches any
// run of bytes, '?' matches one byte, '[...]' matches a class (with '^' for
// negation and 'a-z' for ranges), and '\' escapes the next byte. Matching is
// byte-oriented, not rune-oriented, which is what makes it binary-safe.
func MatchPattern(pattern, s []byte) bool {
	p, t := 0, 0
	for p < len(pattern) && t < len(s) {
		switch pattern[p] {
		case '*':
			// Collapse runs of '*'; a trailing '*' matches everything left.
			for p+1 < len(pattern) && pattern[p+1] == '*' {
				p++
			}
			if p+1 == len(pattern) {
				return true
			}
			for i := t; i <= len(s); i++ {
				if MatchPattern(pattern[p+1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			t++
		case '[':
			var ok bool
			p, ok = matchClass(pattern, p, s[t])
			if !ok {
				return false
			}
			t++
		case '\\':
			if p+1 < len(pattern) {
				p++
			}
			if pattern[p] != s[t] {
				return false
			}
			t++
		default:
			if pattern[p] != s[t] {
				return false
			}
			t++
		}
		p++
		if t == len(s) {
			// Any remaining '*' can absorb the empty tail.
			for p < len(pattern) && pattern[p] == '*' {
				p++
			}
			break
		}
	}
	// A trailing run of '*' can absorb an empty remainder, which is also
	// what makes the pattern "*" match the empty string.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern) && t == len(s)
}

// matchClass evaluates a bracket expression starting at pattern[p] == '[' and
// returns the index of its closing bracket along with whether c matched.
func matchClass(pattern []byte, p int, c byte) (int, bool) {
	p++ // step over '['
	negate := p < len(pattern) && pattern[p] == '^'
	if negate {
		p++
	}
	matched := false
	for {
		switch {
		case p >= len(pattern):
			// Unterminated class: treat the '[' as the last byte consumed.
			return p - 1, matched != negate
		case pattern[p] == '\\' && p+1 < len(pattern):
			p++
			if pattern[p] == c {
				matched = true
			}
		case pattern[p] == ']':
			return p, matched != negate
		case p+2 < len(pattern) && pattern[p+1] == '-' && pattern[p+2] != ']':
			lo, hi := pattern[p], pattern[p+2]
			if lo > hi {
				lo, hi = hi, lo
			}
			if c >= lo && c <= hi {
				matched = true
			}
			p += 2
		default:
			if pattern[p] == c {
				matched = true
			}
		}
		p++
	}
}
