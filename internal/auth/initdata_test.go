package auth

import (
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const testToken = "123456:TEST-token"

// sign builds initData the way Telegram does, so tests don't depend on captured real tokens.
func sign(t *testing.T, values url.Values, exclude ...string) string {
	t.Helper()
	secret := hmacSHA256([]byte("WebAppData"), []byte(testToken))
	values.Set("hash", hex.EncodeToString(hmacSHA256(secret, []byte(DataCheckString(values, append(exclude, "hash")...)))))
	return values.Encode()
}

func baseValues(authDate time.Time) url.Values {
	return url.Values{
		"auth_date":     {strconv.FormatInt(authDate.Unix(), 10)},
		"query_id":      {"AAH1"},
		"chat_instance": {"-42"},
		"user":          {`{"id":777,"first_name":"Анна","last_name":"Петрова","username":"anna","language_code":"ru"}`},
	}
}

func newTestValidator(now time.Time) *Validator {
	v := NewValidator(testToken, 24*time.Hour)
	v.now = func() time.Time { return now }
	return v
}

func TestValidate(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	t.Run("valid", func(t *testing.T) {
		u, err := newTestValidator(now).Validate(sign(t, baseValues(now.Add(-time.Hour))))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.ID != 777 || u.DisplayName() != "Анна Петрова" || u.ChatInstance != "-42" {
			t.Fatalf("got %+v", u)
		}
	})

	t.Run("chat_instance can't be swapped", func(t *testing.T) {
		vals, err := url.ParseQuery(sign(t, baseValues(now)))
		if err != nil {
			t.Fatal(err)
		}
		vals.Set("chat_instance", "-1") // pretend to come from the club's group
		if _, err := newTestValidator(now).Validate(vals.Encode()); !errors.Is(err, ErrBadHash) {
			t.Fatalf("want ErrBadHash, got %v", err)
		}
	})

	t.Run("signature field included in check string", func(t *testing.T) {
		vals := baseValues(now)
		vals.Set("signature", "ed25519-sig")
		if _, err := newTestValidator(now).Validate(sign(t, vals)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("signature field excluded from check string", func(t *testing.T) {
		vals := baseValues(now)
		vals.Set("signature", "ed25519-sig")
		if _, err := newTestValidator(now).Validate(sign(t, vals, "signature")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("tampered user", func(t *testing.T) {
		q, _ := url.ParseQuery(sign(t, baseValues(now)))
		q.Set("user", `{"id":1,"first_name":"Злоумышленник"}`)
		if _, err := newTestValidator(now).Validate(q.Encode()); !errors.Is(err, ErrBadHash) {
			t.Fatalf("want ErrBadHash, got %v", err)
		}
	})

	t.Run("wrong bot token", func(t *testing.T) {
		v := NewValidator("other:token", 24*time.Hour)
		v.now = func() time.Time { return now }
		if _, err := v.Validate(sign(t, baseValues(now))); !errors.Is(err, ErrBadHash) {
			t.Fatalf("want ErrBadHash, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		if _, err := newTestValidator(now).Validate(sign(t, baseValues(now.Add(-25*time.Hour)))); !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %v", err)
		}
	})

	t.Run("missing hash", func(t *testing.T) {
		if _, err := newTestValidator(now).Validate(baseValues(now).Encode()); !errors.Is(err, ErrMissingHash) {
			t.Fatalf("want ErrMissingHash, got %v", err)
		}
	})

	t.Run("no user", func(t *testing.T) {
		vals := baseValues(now)
		vals.Del("user")
		if _, err := newTestValidator(now).Validate(sign(t, vals)); !errors.Is(err, ErrNoUser) {
			t.Fatalf("want ErrNoUser, got %v", err)
		}
	})
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		u    User
		want string
	}{
		{User{FirstName: "Анна"}, "Анна"},
		{User{FirstName: " ", Username: "anna"}, "@anna"},
		{User{}, "Участник клуба"},
	}
	for _, c := range cases {
		if got := c.u.DisplayName(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.u, got, c.want)
		}
	}
}
