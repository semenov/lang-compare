package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

type UUID = [16]byte

// ---------------------------------------------------------------------------------------------------------------
// ids: UUIDv7, monotonic within the process so that id order == creation order

var (
	idMu     sync.Mutex
	idLastMs int64
	idSeq    uint16
)

func newID() UUID {
	var u UUID
	rand.Read(u[8:])
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= idLastMs {
		idSeq++
		if idSeq > 0xFFF {
			idLastMs++
			idSeq = 0
		}
		ms = idLastMs
	} else {
		idLastMs = ms
		idSeq = 0
	}
	seq := idSeq
	idMu.Unlock()
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = 0x70 | byte(seq>>8)
	u[7] = byte(seq)
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

func appendUUID(b []byte, u UUID) []byte {
	var s [36]byte
	hex.Encode(s[0:8], u[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], u[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], u[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], u[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], u[10:16])
	return append(b, s[:]...)
}

func uuidStr(u UUID) string { return string(appendUUID(nil, u)) }

func parseUUID(s string) (UUID, bool) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, false
	}
	j := 0
	for i := 0; i < 36; {
		if s[i] == '-' {
			i++
			continue
		}
		h, ok1 := unhex(s[i])
		l, ok2 := unhex(s[i+1])
		if !ok1 || !ok2 {
			return u, false
		}
		u[j] = h<<4 | l
		j++
		i += 2
	}
	return u, true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func randToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------------------------------------------------------
// time

func nowMs() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func appendTime(b []byte, t time.Time) []byte {
	b = append(b, '"')
	b = t.UTC().AppendFormat(b, "2006-01-02T15:04:05.000Z")
	return append(b, '"')
}

// ---------------------------------------------------------------------------------------------------------------
// JSON output

const hexDigits = "0123456789abcdef"

func appendStr(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

func appendStrList(b []byte, l []string) []byte {
	b = append(b, '[')
	for i, s := range l {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendStr(b, s)
	}
	return append(b, ']')
}

func appendInt(b []byte, n int64) []byte { return strconv.AppendInt(b, n, 10) }

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"application/json"}
	h["Content-Length"] = []string{strconv.Itoa(len(body))}
	w.WriteHeader(status)
	w.Write(body)
}

func writeAny(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeJSON(w, status, b)
}

// ---------------------------------------------------------------------------------------------------------------
// errors

type fieldErr struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

type verrs []fieldErr

func (v *verrs) add(field, code string) { *v = append(*v, fieldErr{field, code}) }

func writeProblem(w http.ResponseWriter, status int, code, detail string, errs []fieldErr) {
	b := make([]byte, 0, 128)
	b = append(b, `{"status":`...)
	b = appendInt(b, int64(status))
	b = append(b, `,"code":`...)
	b = appendStr(b, code)
	b = append(b, `,"detail":`...)
	b = appendStr(b, detail)
	if errs != nil {
		b = append(b, `,"errors":[`...)
		for i, e := range errs {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"field":`...)
			b = appendStr(b, e.Field)
			b = append(b, `,"code":`...)
			b = appendStr(b, e.Code)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	b = append(b, '}')
	h := w.Header()
	h["Content-Type"] = []string{"application/problem+json"}
	h["Content-Length"] = []string{strconv.Itoa(len(b))}
	w.WriteHeader(status)
	w.Write(b)
}

func errNotFound(w http.ResponseWriter) { writeProblem(w, 404, "not_found", "not found", nil) }
func errForbidden(w http.ResponseWriter) {
	writeProblem(w, 403, "forbidden", "not allowed", nil)
}
func errBadRequest(w http.ResponseWriter, d string) { writeProblem(w, 400, "bad_request", d, nil) }
func errValidation(w http.ResponseWriter, errs verrs) {
	writeProblem(w, 422, "validation_failed", "validation failed", errs)
}
func errValidation1(w http.ResponseWriter, field, code string) {
	errValidation(w, verrs{{field, code}})
}
func errConflict(w http.ResponseWriter, code, d string) { writeProblem(w, 409, code, d, nil) }
func errInternal(w http.ResponseWriter, err error) {
	logf("internal error: %v", err)
	writeProblem(w, 500, "internal", "internal error", nil)
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---------------------------------------------------------------------------------------------------------------
// JSON input

const maxBody = 128 << 20

// decodeBody reads and decodes the request body into dst (a struct of json.RawMessage fields).
// Returns false after writing a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		errBadRequest(w, "cannot read body")
		return nil, false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		errBadRequest(w, "malformed JSON")
		return nil, false
	}
	return raw, true
}

func isNull(r json.RawMessage) bool { return len(r) == 4 && string(r) == "null" }

// str parses an optional string field. present=false if absent; ok=false if not a string (null counts as not ok
// unless nullOK).
func str(raw json.RawMessage) (s string, isStr bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// reqString validates a required string with length limits (after optional trimming).
func reqString(v *verrs, field string, raw json.RawMessage, trim bool, min, max int) (string, bool) {
	if len(raw) == 0 {
		v.add(field, "required")
		return "", false
	}
	if isNull(raw) {
		v.add(field, "required")
		return "", false
	}
	s, ok := str(raw)
	if !ok {
		v.add(field, "invalid")
		return "", false
	}
	if trim {
		s = strings.TrimSpace(s)
	}
	n := runeLen(s)
	if n < min {
		if n == 0 {
			v.add(field, "required")
		} else {
			v.add(field, "too_short")
		}
		return "", false
	}
	if n > max {
		v.add(field, "too_long")
		return "", false
	}
	return s, true
}

// ---------------------------------------------------------------------------------------------------------------
// words for full-text search: maximal runs of Unicode letters and decimal digits, lower-cased

func wordsOf(dst map[string]struct{}, s string) {
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			dst[strings.ToLower(s[start:i])] = struct{}{}
			start = -1
		}
	}
	if start >= 0 {
		dst[strings.ToLower(s[start:])] = struct{}{}
	}
}

func issueWords(title string, desc *string) []string {
	m := make(map[string]struct{}, 16)
	wordsOf(m, title)
	if desc != nil {
		wordsOf(m, *desc)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func queryWords(q string) []string {
	m := make(map[string]struct{})
	wordsOf(m, q)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------------------------------------------
// pagination

func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 50, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 100 {
		errValidation1(w, "limit", "out_of_range")
		return 0, false
	}
	return n, true
}

func encCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func decCursor(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return string(b), err == nil
}

// page writes {"items": [...], "next_cursor": ...} from pre-rendered item JSON.
func appendPage(b []byte, items [][]byte, next string) []byte {
	b = append(b, `{"items":[`...)
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	b = append(b, `],"next_cursor":`...)
	if next == "" {
		b = append(b, "null"...)
	} else {
		b = appendStr(b, encCursor(next))
	}
	return append(b, '}')
}
