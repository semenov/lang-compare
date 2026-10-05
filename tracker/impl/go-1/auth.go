package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"
)

// ---------------------------------------------------------------------------------------------------------------
// password hashing: Argon2id m=19456 KiB, t=2, p=1; concurrency bounded to keep memory in check

const (
	argonM = 19456
	argonT = 2
	argonP = 1
)

var argonSem chan struct{}

func initArgon() {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	argonSem = make(chan struct{}, n)
}

func argonKey(pw string, salt []byte) []byte {
	argonSem <- struct{}{}
	defer func() { <-argonSem }()
	return argon2.IDKey([]byte(pw), salt, argonT, argonM, argonP, 32)
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argonKey(pw, salt)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)
}

func checkPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, _ := strings.Cut(kv, "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "m":
			m = uint32(n)
		case "t":
			t = uint32(n)
		case "p":
			p = uint8(n)
		}
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	var key []byte
	if m == argonM && t == argonT && p == argonP {
		key = argonKey(pw, salt)
	} else {
		argonSem <- struct{}{}
		key = argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
		<-argonSem
	}
	return subtle.ConstantTimeCompare(key, want) == 1
}

// ---------------------------------------------------------------------------------------------------------------
// JWT (HS256)

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func makeAccess(uid UUID) string {
	now := time.Now().Unix()
	p := make([]byte, 0, 128)
	p = append(p, `{"sub":"`...)
	p = appendUUID(p, uid)
	p = append(p, `","iat":`...)
	p = appendInt(p, now)
	p = append(p, `,"exp":`...)
	p = appendInt(p, now+900)
	p = append(p, `,"typ":"access"}`...)
	s := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(p)
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(s))
	return s + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

var errExpired = errors.New("expired")

func verifyAccess(tok string) (UUID, error) {
	var zero UUID
	bad := errors.New("invalid")
	i := strings.IndexByte(tok, '.')
	j := strings.LastIndexByte(tok, '.')
	if i < 0 || j <= i {
		return zero, bad
	}
	hb, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return zero, bad
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256" {
		return zero, bad
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[j+1:])
	if err != nil {
		return zero, bad
	}
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(tok[:j]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return zero, bad
	}
	pb, err := base64.RawURLEncoding.DecodeString(tok[i+1 : j])
	if err != nil {
		return zero, bad
	}
	var cl struct {
		Sub string   `json:"sub"`
		Exp *float64 `json:"exp"`
		Typ string   `json:"typ"`
	}
	if json.Unmarshal(pb, &cl) != nil || cl.Typ != "access" || cl.Exp == nil {
		return zero, bad
	}
	uid, ok := parseUUID(cl.Sub)
	if !ok {
		return zero, bad
	}
	if float64(time.Now().Unix()) >= *cl.Exp {
		return zero, errExpired
	}
	return uid, nil
}

type authedHandler func(w http.ResponseWriter, r *http.Request, uid UUID)

func authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		if len(a) < 7 || !strings.EqualFold(a[:7], "bearer ") {
			writeProblem(w, 401, "unauthenticated", "missing bearer token", nil)
			return
		}
		uid, err := verifyAccess(strings.TrimSpace(a[7:]))
		if err == errExpired {
			writeProblem(w, 401, "token_expired", "access token expired", nil)
			return
		}
		if err != nil {
			writeProblem(w, 401, "unauthenticated", "invalid access token", nil)
			return
		}
		h(w, r, uid)
	}
}

// ---------------------------------------------------------------------------------------------------------------
// users

func appendUser(b []byte, id UUID, email, name string, created time.Time) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, id)
	b = append(b, `","email":`...)
	b = appendStr(b, email)
	b = append(b, `,"name":`...)
	b = appendStr(b, name)
	b = append(b, `,"created_at":`...)
	b = appendTime(b, created)
	return append(b, '}')
}

func validEmail(v *verrs, raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isNull(raw) {
		v.add("email", "required")
		return "", false
	}
	s, ok := str(raw)
	if !ok {
		v.add("email", "invalid")
		return "", false
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		v.add("email", "required")
		return "", false
	}
	if runeLen(s) > 254 {
		v.add("email", "too_long")
		return "", false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.ContainsAny(s, " \t\r\n") {
		v.add("email", "invalid")
		return "", false
	}
	return s, true
}

func hRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, Password, Name json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	var v verrs
	email, _ := validEmail(&v, in.Email)
	pw, _ := reqString(&v, "password", in.Password, false, 10, 128)
	name, _ := reqString(&v, "name", in.Name, true, 1, 100)
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	hash := hashPassword(pw)
	id := newID()
	now := nowMs()
	_, err := db.Exec(r.Context(), "INSERT INTO users (id, email, name, pw, created_at) VALUES ($1,$2,$3,$4,$5)",
		id, email, name, hash, now)
	if err != nil {
		if isUnique(err) {
			errConflict(w, "email_taken", "email already registered")
			return
		}
		errInternal(w, err)
		return
	}
	writeJSON(w, 201, appendUser(nil, id, email, name, now))
}

func tokenHash(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// newPair creates a refresh token in the given chain and returns the token response body.
func newPair(ctx context.Context, uid, chain UUID) ([]byte, error) {
	rt := randToken(32)
	_, err := db.Exec(ctx, "INSERT INTO refresh_tokens (hash, chain, user_id, expires_at) VALUES ($1,$2,$3,$4)",
		tokenHash(rt), chain, uid, time.Now().Add(30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, 400)
	b = append(b, `{"access_token":`...)
	b = appendStr(b, makeAccess(uid))
	b = append(b, `,"refresh_token":`...)
	b = appendStr(b, rt)
	b = append(b, `,"token_type":"Bearer","expires_in":900}`...)
	return b, nil
}

func hLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, Password json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	email, ok1 := str(in.Email)
	pw, ok2 := str(in.Password)
	if !ok1 || !ok2 {
		var v verrs
		if !ok1 {
			v.add("email", "required")
		}
		if !ok2 {
			v.add("password", "required")
		}
		errValidation(w, v)
		return
	}
	email = strings.ToLower(strings.TrimSpace(email))
	var uid UUID
	var hash string
	err := db.QueryRow(r.Context(), "SELECT id, pw FROM users WHERE email=$1", email).Scan(&uid, &hash)
	if err != nil && err != pgx.ErrNoRows {
		errInternal(w, err)
		return
	}
	if err == pgx.ErrNoRows || !checkPassword(pw, hash) {
		writeProblem(w, 401, "invalid_credentials", "wrong email or password", nil)
		return
	}
	body, err := newPair(r.Context(), uid, newID())
	if err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, body)
}

func readRefresh(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		RefreshToken json.RawMessage `json:"refresh_token"`
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return "", false
	}
	s, ok := str(in.RefreshToken)
	if !ok || s == "" {
		errValidation1(w, "refresh_token", "required")
		return "", false
	}
	return s, true
}

func hRefresh(w http.ResponseWriter, r *http.Request) {
	tok, ok := readRefresh(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	h := tokenHash(tok)
	var uid, chain UUID
	err := db.QueryRow(ctx, `UPDATE refresh_tokens SET used=true
		WHERE hash=$1 AND NOT used AND NOT revoked AND expires_at > now() RETURNING user_id, chain`, h).Scan(&uid, &chain)
	if err == nil {
		body, err := newPair(ctx, uid, chain)
		if err != nil {
			errInternal(w, err)
			return
		}
		writeJSON(w, 200, body)
		return
	}
	if err != pgx.ErrNoRows {
		errInternal(w, err)
		return
	}
	var used, revoked bool
	var exp time.Time
	err = db.QueryRow(ctx, "SELECT chain, used, revoked, expires_at FROM refresh_tokens WHERE hash=$1", h).
		Scan(&chain, &used, &revoked, &exp)
	if err == nil && used && !revoked {
		db.Exec(ctx, "UPDATE refresh_tokens SET revoked=true WHERE chain=$1", chain)
		writeProblem(w, 401, "token_reused", "refresh token reuse detected", nil)
		return
	}
	if err != nil && err != pgx.ErrNoRows {
		errInternal(w, err)
		return
	}
	writeProblem(w, 401, "invalid_token", "invalid refresh token", nil)
}

func hLogout(w http.ResponseWriter, r *http.Request) {
	tok, ok := readRefresh(w, r)
	if !ok {
		return
	}
	_, err := db.Exec(r.Context(), `UPDATE refresh_tokens SET revoked=true
		WHERE chain=(SELECT chain FROM refresh_tokens WHERE hash=$1)`, tokenHash(tok))
	if err != nil {
		errInternal(w, err)
		return
	}
	w.WriteHeader(204)
}

func hMe(w http.ResponseWriter, r *http.Request, uid UUID) {
	var email, name string
	var created time.Time
	err := db.QueryRow(r.Context(), "SELECT email, name, created_at FROM users WHERE id=$1", uid).
		Scan(&email, &name, &created)
	if err == pgx.ErrNoRows {
		writeProblem(w, 401, "unauthenticated", "unknown user", nil)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendUser(nil, uid, email, name, created))
}

func hPatchMe(w http.ResponseWriter, r *http.Request, uid UUID) {
	var in struct {
		Name json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	var email, name string
	var created time.Time
	var err error
	if len(in.Name) > 0 {
		var v verrs
		n, _ := reqString(&v, "name", in.Name, true, 1, 100)
		if len(v) > 0 {
			errValidation(w, v)
			return
		}
		err = db.QueryRow(r.Context(), "UPDATE users SET name=$2 WHERE id=$1 RETURNING email, name, created_at",
			uid, n).Scan(&email, &name, &created)
	} else {
		err = db.QueryRow(r.Context(), "SELECT email, name, created_at FROM users WHERE id=$1", uid).
			Scan(&email, &name, &created)
	}
	if err == pgx.ErrNoRows {
		writeProblem(w, 401, "unauthenticated", "unknown user", nil)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendUser(nil, uid, email, name, created))
}
