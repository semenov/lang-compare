package main

import (
	"encoding/binary"
	"math/bits"
	"unicode"
	"unicode/utf8"
)

const (
	numBuckets = 16  // term-space partitions (hash of term)
	blockSize  = 128 // docs per postings block
	endDoc     = ^uint32(0)
)

// ---- varints ----

func putUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// uvarint decodes a varint at b[i:], returning value and new offset.
func uvarint(b []byte, i int) (uint64, int) {
	c := b[i]
	if c < 0x80 {
		return uint64(c), i + 1
	}
	v := uint64(c & 0x7f)
	s := uint(7)
	i++
	for {
		c = b[i]
		i++
		if c < 0x80 {
			return v | uint64(c)<<s, i
		}
		v |= uint64(c&0x7f) << s
		s += 7
	}
}

// skipVarints skips n varints starting at b[i:].
func skipVarints(b []byte, i int, n int) int {
	for n >= 8 && i+8 <= len(b) {
		w := binary.LittleEndian.Uint64(b[i:])
		c := bits.OnesCount64(^w & 0x8080808080808080)
		if c >= n {
			break
		}
		n -= c
		i += 8
	}
	for n > 0 {
		if b[i] < 0x80 {
			n--
		}
		i++
	}
	return i
}

// ---- hashing (stable across processes) ----

func termBucket(t []byte) int {
	h := uint64(14695981039346656037)
	for _, c := range t {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return int(h % numBuckets)
}

// ---- tokenizer ----

var asciiTab [128]byte

func init() {
	for c := 0; c < 128; c++ {
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
			asciiTab[c] = byte(c)
		case 'A' <= c && c <= 'Z':
			asciiTab[c] = byte(c + 32)
		}
	}
}

type tokenizer struct {
	s   []byte
	i   int
	buf []byte
}

func (t *tokenizer) reset(s []byte) { t.s = s; t.i = 0 }

// next returns the next lowercased token (valid until the next call).
func (t *tokenizer) next() ([]byte, bool) {
	s := t.s
	i := t.i
	buf := t.buf[:0]
	for i < len(s) {
		c := s[i]
		if c < 0x80 {
			i++
			if l := asciiTab[c]; l != 0 {
				buf = append(buf, l)
				continue
			}
			if len(buf) > 0 {
				t.i, t.buf = i, buf
				return buf, true
			}
			continue
		}
		r, sz := utf8.DecodeRune(s[i:])
		i += sz
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			buf = utf8.AppendRune(buf, unicode.ToLower(r))
			continue
		}
		if len(buf) > 0 {
			t.i, t.buf = i, buf
			return buf, true
		}
	}
	t.i, t.buf = i, buf
	if len(buf) > 0 {
		return buf, true
	}
	return nil, false
}

func tokenizeString(s string) []string {
	var t tokenizer
	t.reset([]byte(s))
	var out []string
	for {
		tok, ok := t.next()
		if !ok {
			return out
		}
		out = append(out, string(tok))
	}
}

// ---- minimal JSON line parsing ----

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// scanString: b[i] == '"'; returns raw content [i+1:end) and index after closing quote, and whether escapes are present.
func scanString(b []byte, i int) (raw []byte, next int, esc bool, ok bool) {
	j := i + 1
	for j < len(b) {
		switch b[j] {
		case '"':
			return b[i+1 : j], j + 1, esc, true
		case '\\':
			esc = true
			j += 2
			continue
		}
		j++
	}
	return nil, 0, false, false
}

func hexVal(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case '0' <= c && c <= '9':
			r |= rune(c - '0')
		case 'a' <= c && c <= 'f':
			r |= rune(c - 'a' + 10)
		case 'A' <= c && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

// unescape decodes JSON string content into dst.
func unescape(dst, raw []byte) []byte {
	for i := 0; i < len(raw); {
		c := raw[i]
		if c != '\\' {
			j := i + 1
			for j < len(raw) && raw[j] != '\\' {
				j++
			}
			dst = append(dst, raw[i:j]...)
			i = j
			continue
		}
		if i+1 >= len(raw) {
			break
		}
		e := raw[i+1]
		i += 2
		switch e {
		case 'n':
			dst = append(dst, '\n')
		case 't':
			dst = append(dst, '\t')
		case 'r':
			dst = append(dst, '\r')
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'u':
			r, ok := hexVal(raw[i:])
			if !ok {
				dst = append(dst, "�"...)
				continue
			}
			i += 4
			if r >= 0xD800 && r < 0xDC00 {
				if i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
					if r2, ok := hexVal(raw[i+2:]); ok && r2 >= 0xDC00 && r2 < 0xE000 {
						i += 6
						r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
						dst = utf8.AppendRune(dst, r)
						continue
					}
				}
				r = 0xFFFD
			} else if r >= 0xDC00 && r < 0xE000 {
				r = 0xFFFD
			}
			dst = utf8.AppendRune(dst, r)
		default:
			dst = append(dst, e)
		}
	}
	return dst
}

func skipValue(b []byte, i int) (int, bool) {
	for i < len(b) && isWS(b[i]) {
		i++
	}
	if i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case '"':
		_, n, _, ok := scanString(b, i)
		return n, ok
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				_, n, _, ok := scanString(b, i)
				if !ok {
					return 0, false
				}
				i = n
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return 0, false
	default:
		for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && !isWS(b[i]) {
			i++
		}
		return i, true
	}
}

type rawDoc struct {
	id       uint32
	title    []byte // raw JSON string content
	text     []byte
	titleEsc bool
	textEsc  bool
}

func parseLine(b []byte) (d rawDoc, ok bool) {
	i := 0
	for i < len(b) && isWS(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '{' {
		return d, false
	}
	i++
	for {
		for i < len(b) && (isWS(b[i]) || b[i] == ',') {
			i++
		}
		if i >= len(b) {
			return d, false
		}
		if b[i] == '}' {
			return d, true
		}
		if b[i] != '"' {
			return d, false
		}
		key, n, _, ok := scanString(b, i)
		if !ok {
			return d, false
		}
		i = n
		for i < len(b) && isWS(b[i]) {
			i++
		}
		if i >= len(b) || b[i] != ':' {
			return d, false
		}
		i++
		for i < len(b) && isWS(b[i]) {
			i++
		}
		if i >= len(b) {
			return d, false
		}
		switch string(key) {
		case "id":
			var v uint64
			for i < len(b) && b[i] >= '0' && b[i] <= '9' {
				v = v*10 + uint64(b[i]-'0')
				i++
			}
			d.id = uint32(v)
		case "title", "text":
			if b[i] != '"' {
				return d, false
			}
			raw, n, esc, ok := scanString(b, i)
			if !ok {
				return d, false
			}
			i = n
			if key[1] == 'i' {
				d.title, d.titleEsc = raw, esc
			} else {
				d.text, d.textEsc = raw, esc
			}
		default:
			n, ok := skipValue(b, i)
			if !ok {
				return d, false
			}
			i = n
		}
	}
}

func isBlank(line []byte) bool {
	for _, c := range line {
		if !isWS(c) {
			return false
		}
	}
	return true
}
