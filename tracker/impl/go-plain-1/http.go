package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- errors

type fieldErr struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

type apiError struct {
	Status int
	Code   string
	Detail string
	Errors []fieldErr
}

func (e *apiError) Error() string { return e.Code + ": " + e.Detail }

func newErr(status int, code, detail string) error {
	return &apiError{Status: status, Code: code, Detail: detail}
}

func notFound() error   { return newErr(404, "not_found", "resource not found") }
func forbidden() error  { return newErr(403, "forbidden", "you may not do this") }
func badRequest() error { return newErr(400, "bad_request", "malformed JSON body") }

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal: %v", err)
		w.WriteHeader(500)
		return
	}
	writeRaw(w, status, "application/json", b)
}

func writeRaw(w http.ResponseWriter, status int, ctype string, b []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

func writeProblem(w http.ResponseWriter, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		log.Printf("internal error: %v", err)
		e = &apiError{Status: 500, Code: "internal", Detail: "internal server error"}
	}
	m := map[string]any{"status": e.Status, "code": e.Code, "detail": e.Detail}
	if e.Errors != nil {
		m["errors"] = e.Errors
	}
	b, _ := json.Marshal(m)
	writeRaw(w, e.Status, "application/problem+json", b)
}

type hfunc func(w http.ResponseWriter, r *http.Request, uid string) error

func (s *Server) handle(mux *http.ServeMux, pattern string, auth bool, h hfunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic in %s %s: %v", r.Method, r.URL.Path, rec)
				writeProblem(w, fmt.Errorf("panic: %v", rec))
			}
		}()
		uid := ""
		if auth {
			id, err := s.authenticate(r)
			if err != nil {
				writeProblem(w, err)
				return
			}
			uid = id
		}
		if err := h(w, r, uid); err != nil {
			writeProblem(w, err)
		}
	})
}

// ---- request bodies & validation

type obj map[string]json.RawMessage

func readObj(r *http.Request) (obj, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 128<<20))
	if err != nil {
		return nil, nil, badRequest()
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return obj{}, raw, nil
	}
	var m obj
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, nil, badRequest()
	}
	return m, raw, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

type verrs struct {
	prefix string
	list   []fieldErr
}

func (v *verrs) add(field, code string) {
	v.list = append(v.list, fieldErr{Field: v.prefix + field, Code: code})
}
func (v *verrs) ok() bool { return len(v.list) == 0 }
func (v *verrs) err() error {
	return &apiError{Status: 422, Code: "validation_failed", Detail: "request validation failed", Errors: v.list}
}

const (
	fAbsent = iota
	fNull
	fSet
	fBad
)

type strSpec struct {
	required, nullable, trim bool
	min, max                 int
}

func (v *verrs) str(o obj, key string, sp strSpec) (string, int) {
	raw, ok := o[key]
	if !ok {
		if sp.required {
			v.add(key, "required")
		}
		return "", fAbsent
	}
	if isNull(raw) {
		if sp.nullable {
			return "", fNull
		}
		v.add(key, "required")
		return "", fBad
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		v.add(key, "invalid")
		return "", fBad
	}
	if sp.trim {
		s = strings.TrimSpace(s)
	}
	n := utf8.RuneCountInString(s)
	if n < sp.min {
		v.add(key, "too_short")
		return "", fBad
	}
	if sp.max > 0 && n > sp.max {
		v.add(key, "too_long")
		return "", fBad
	}
	return s, fSet
}

func (v *verrs) enum(o obj, key string, required bool, allowed ...string) (string, int) {
	s, st := v.str(o, key, strSpec{required: required})
	if st == fSet && !contains(allowed, s) {
		v.add(key, "invalid")
		return "", fBad
	}
	return s, st
}

func (v *verrs) boolean(o obj, key string) (bool, int) {
	raw, ok := o[key]
	if !ok {
		return false, fAbsent
	}
	var b bool
	if isNull(raw) || json.Unmarshal(raw, &b) != nil {
		v.add(key, "invalid")
		return false, fBad
	}
	return b, fSet
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- pagination (opaque offset cursors)

type page struct {
	limit, offset int
}

func parsePage(q url.Values, v *verrs) page {
	p := page{limit: 50}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		switch {
		case err != nil:
			v.add("limit", "invalid")
		case n < 1 || n > 100:
			v.add("limit", "out_of_range")
		default:
			p.limit = n
		}
	}
	if c := q.Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		n, err2 := strconv.Atoi(strings.TrimPrefix(string(b), "o:"))
		if err != nil || err2 != nil || !strings.HasPrefix(string(b), "o:") || n < 0 {
			v.add("cursor", "invalid")
		} else {
			p.offset = n
		}
	}
	return p
}

func pageFromRequest(r *http.Request) (page, error) {
	v := &verrs{}
	p := parsePage(r.URL.Query(), v)
	if !v.ok() {
		return p, v.err()
	}
	return p, nil
}

// sql returns the LIMIT/OFFSET clause (fetching one extra row to detect more).
func (p page) sql() string {
	return fmt.Sprintf(" LIMIT %d OFFSET %d", p.limit+1, p.offset)
}

// finish trims the extra row and returns the list response.
func finish[T any](p page, items []T) map[string]any {
	var next *string
	if len(items) > p.limit {
		items = items[:p.limit]
		c := base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(p.offset+p.limit)))
		next = &c
	}
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items, "next_cursor": next}
}

// ---- misc

const tsLayout = "2006-01-02T15:04:05.000Z"

func nowMS() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func fmtTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func fmtTSPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := fmtTS(*t)
	return &s
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidRe.MatchString(s) }
