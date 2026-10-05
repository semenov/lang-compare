package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"
)

const (
	accessTTL  = 900
	refreshTTL = 30 * 24 * time.Hour
)

// ---- passwords

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	h := argon2.IDKey([]byte(pw), salt, 2, 19456, 1, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", enc.EncodeToString(salt), enc.EncodeToString(h))
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var dummyHash = hashPassword("dummy-password-for-timing")

// ---- JWT

var b64u = base64.RawURLEncoding

func (s *Server) signAccess(uid string, now time.Time) string {
	h := b64u.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	c, _ := json.Marshal(map[string]any{"sub": uid, "iat": now.Unix(), "exp": now.Unix() + accessTTL, "typ": "access"})
	p := b64u.EncodeToString(c)
	mac := hmac.New(sha256.New, s.jwtSecret)
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + b64u.EncodeToString(mac.Sum(nil))
}

func unauthenticated() error {
	return newErr(401, "unauthenticated", "missing or invalid access token")
}

func (s *Server) authenticate(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", unauthenticated()
	}
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return "", unauthenticated()
	}
	hb, err := b64u.DecodeString(parts[0])
	if err != nil {
		return "", unauthenticated()
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256" {
		return "", unauthenticated()
	}
	sig, err := b64u.DecodeString(parts[2])
	if err != nil {
		return "", unauthenticated()
	}
	mac := hmac.New(sha256.New, s.jwtSecret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", unauthenticated()
	}
	pb, err := b64u.DecodeString(parts[1])
	if err != nil {
		return "", unauthenticated()
	}
	var claims struct {
		Sub string   `json:"sub"`
		Exp *float64 `json:"exp"`
		Typ string   `json:"typ"`
	}
	if json.Unmarshal(pb, &claims) != nil || claims.Typ != "access" || !isUUID(claims.Sub) || claims.Exp == nil {
		return "", unauthenticated()
	}
	if float64(time.Now().Unix()) >= *claims.Exp {
		return "", newErr(401, "token_expired", "access token expired")
	}
	return strings.ToLower(claims.Sub), nil
}

// ---- refresh tokens

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Server) issueTokens(ctx context.Context, q querier, uid, chain string) (map[string]any, error) {
	now := time.Now()
	raw := make([]byte, 32)
	rand.Read(raw)
	rt := b64u.EncodeToString(raw)
	if _, err := q.Exec(ctx, `INSERT INTO refresh_tokens (token_hash, user_id, chain_id, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(rt), uid, chain, now.Add(refreshTTL)); err != nil {
		return nil, err
	}
	return map[string]any{
		"access_token":  s.signAccess(uid, now),
		"refresh_token": rt,
		"token_type":    "Bearer",
		"expires_in":    accessTTL,
	}, nil
}

// ---- handlers

type User struct {
	ID        string
	Email     string
	Name      string
	CreatedAt time.Time
}

func (u *User) JSON() map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "name": u.Name, "created_at": fmtTS(u.CreatedAt)}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request, _ string) error {
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	email, st := v.str(o, "email", strSpec{required: true, trim: true, max: 254})
	if st == fSet && !strings.Contains(email, "@") {
		v.add("email", "invalid")
	}
	pw, _ := v.str(o, "password", strSpec{required: true, min: 10, max: 128})
	name, _ := v.str(o, "name", strSpec{required: true, trim: true, min: 1, max: 100})
	if !v.ok() {
		return v.err()
	}
	u := &User{ID: newID(), Email: strings.ToLower(email), Name: name, CreatedAt: nowMS()}
	tag, err := s.pool.Exec(r.Context(), `INSERT INTO users (id, email, name, password_hash, created_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (email) DO NOTHING`, u.ID, u.Email, u.Name, hashPassword(pw), u.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return newErr(409, "email_taken", "email already registered")
	}
	writeJSON(w, 201, u.JSON())
	return nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, _ string) error {
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	email, _ := v.str(o, "email", strSpec{required: true, trim: true})
	pw, _ := v.str(o, "password", strSpec{required: true})
	if !v.ok() {
		return v.err()
	}
	var uid, hash string
	err = s.pool.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1`, strings.ToLower(email)).Scan(&uid, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		checkPassword(dummyHash, pw)
		return newErr(401, "invalid_credentials", "wrong email or password")
	} else if err != nil {
		return err
	}
	if !checkPassword(hash, pw) {
		return newErr(401, "invalid_credentials", "wrong email or password")
	}
	resp, err := s.issueTokens(r.Context(), s.pool, uid, newID())
	if err != nil {
		return err
	}
	writeJSON(w, 200, resp)
	return nil
}

func invalidToken() error { return newErr(401, "invalid_token", "refresh token is invalid") }

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, _ string) error {
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	rt, _ := v.str(o, "refresh_token", strSpec{required: true})
	if !v.ok() {
		return v.err()
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var uid, chain string
	var exp time.Time
	var used, revoked bool
	err = tx.QueryRow(ctx, `SELECT user_id, chain_id, expires_at, used, revoked FROM refresh_tokens
		WHERE token_hash = $1 FOR UPDATE`, hashToken(rt)).Scan(&uid, &chain, &exp, &used, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidToken()
	} else if err != nil {
		return err
	}
	if used {
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = true WHERE chain_id = $1`, chain); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return newErr(401, "token_reused", "refresh token was already used")
	}
	if revoked || time.Now().After(exp) {
		return invalidToken()
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET used = true WHERE token_hash = $1`, hashToken(rt)); err != nil {
		return err
	}
	resp, err := s.issueTokens(ctx, tx, uid, chain)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	writeJSON(w, 200, resp)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ string) error {
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	rt, _ := v.str(o, "refresh_token", strSpec{required: true})
	if !v.ok() {
		return v.err()
	}
	tag, err := s.pool.Exec(r.Context(), `UPDATE refresh_tokens SET revoked = true WHERE chain_id =
		(SELECT chain_id FROM refresh_tokens WHERE token_hash = $1)`, hashToken(rt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return invalidToken()
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) loadUser(ctx context.Context, uid string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx, `SELECT id, email, name, created_at FROM users WHERE id = $1`, uid).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, unauthenticated()
	}
	return u, err
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request, uid string) error {
	u, err := s.loadUser(r.Context(), uid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, u.JSON())
	return nil
}

func (s *Server) patchMe(w http.ResponseWriter, r *http.Request, uid string) error {
	u, err := s.loadUser(r.Context(), uid)
	if err != nil {
		return err
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	name, st := v.str(o, "name", strSpec{trim: true, min: 1, max: 100})
	if !v.ok() {
		return v.err()
	}
	if st == fSet {
		if _, err := s.pool.Exec(r.Context(), `UPDATE users SET name = $2 WHERE id = $1`, uid, name); err != nil {
			return err
		}
		u.Name = name
	}
	writeJSON(w, 200, u.JSON())
	return nil
}
