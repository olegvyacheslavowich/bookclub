// Package auth validates the initData string that Telegram passes to a Mini App.
//
// Algorithm (core.telegram.org/bots/webapps, "Validating data received via the Mini App"):
//
//	secret_key        = HMAC_SHA256(key = "WebAppData", msg = bot_token)
//	data_check_string = all received fields except hash, sorted by key, "key=value" joined with "\n"
//	valid             = hex(HMAC_SHA256(key = secret_key, msg = data_check_string)) == hash
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMissingHash = errors.New("initData: hash is missing")
	ErrBadHash     = errors.New("initData: hash mismatch")
	ErrExpired     = errors.New("initData: auth_date is too old")
	ErrNoUser      = errors.New("initData: user is missing")
)

// User is the subset of WebAppUser the app needs, plus where the app was opened from.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`

	// ChatInstance identifies the chat the Mini App was opened from; the same for everyone in that
	// chat. Telegram sends it (and ChatType) only for launches from a direct link, t.me/<bot>/<app>.
	// Both are covered by the signature, so they can't be forged.
	ChatInstance string `json:"-"`
	ChatType     string `json:"-"` // "group", "supergroup", "private", "sender", "channel"
}

// DisplayName is what other club members see next to a book or review.
func (u User) DisplayName() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case name != "":
		return name
	case u.Username != "":
		return "@" + u.Username
	default:
		return "Участник клуба"
	}
}

type Validator struct {
	secret []byte
	maxAge time.Duration
	now    func() time.Time
}

// NewValidator derives the HMAC key from the bot token once.
// maxAge <= 0 disables the auth_date freshness check (not recommended).
func NewValidator(botToken string, maxAge time.Duration) *Validator {
	return &Validator{secret: hmacSHA256([]byte("WebAppData"), []byte(botToken)), maxAge: maxAge, now: time.Now}
}

// Validate checks the signature and freshness of raw initData and returns the user who opened the app.
func (v *Validator) Validate(initData string) (User, error) {
	values, err := url.ParseQuery(initData)
	if err != nil {
		return User{}, fmt.Errorf("initData: parse: %w", err)
	}

	hash := values.Get("hash")
	if hash == "" {
		return User{}, ErrMissingHash
	}
	want, err := hex.DecodeString(hash)
	if err != nil {
		return User{}, ErrBadHash
	}

	// Since Bot API 8.0 initData also carries an Ed25519 "signature" for third-party validation.
	// The bot-token check covers every field except "hash"; the second variant tolerates clients
	// that leave "signature" out of the check string. Both are keyed by the bot token, so
	// accepting either one does not let anyone forge data.
	if !v.signed(values, want, "hash") &&
		!(values.Has("signature") && v.signed(values, want, "hash", "signature")) {
		return User{}, ErrBadHash
	}

	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil {
		return User{}, fmt.Errorf("initData: bad auth_date: %w", err)
	}
	if v.maxAge > 0 && v.now().Sub(time.Unix(authDate, 0)) > v.maxAge {
		return User{}, ErrExpired
	}

	var u User
	if raw := values.Get("user"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return User{}, fmt.Errorf("initData: bad user: %w", err)
		}
	}
	if u.ID == 0 {
		return User{}, ErrNoUser
	}
	u.ChatInstance, u.ChatType = values.Get("chat_instance"), values.Get("chat_type")
	return u, nil
}

func (v *Validator) signed(values url.Values, want []byte, exclude ...string) bool {
	return hmac.Equal(hmacSHA256(v.secret, []byte(DataCheckString(values, exclude...))), want)
}

// DataCheckString builds the string Telegram signs. Exported for tests and tooling.
func DataCheckString(values url.Values, exclude ...string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		if !slices.Contains(exclude, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values.Get(k))
	}
	return b.String()
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
