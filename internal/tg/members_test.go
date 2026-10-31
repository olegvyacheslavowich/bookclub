package tg

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "123456:SECRET"

func fakeTelegram(t *testing.T, calls *atomic.Int32, answer func(userID string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/bot"+token+"/getChatMember" || r.URL.Query().Get("chat_id") != "-100500" {
			t.Errorf("unexpected request %s", r.URL)
		}
		body := answer(r.URL.Query().Get("user_id"))
		if strings.Contains(body, `"ok":false`) {
			w.WriteHeader(http.StatusBadRequest)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checker(baseURL string, now *time.Time) *MemberChecker {
	c := NewMemberChecker("", token, "-100500")
	c.baseURL = baseURL
	c.now = func() time.Time { return *now }
	return c
}

func TestIsMember(t *testing.T) {
	var calls atomic.Int32
	srv := fakeTelegram(t, &calls, func(id string) string {
		switch id {
		case "1":
			return `{"ok":true,"result":{"status":"member"}}`
		case "2":
			return `{"ok":true,"result":{"status":"left"}}`
		case "3":
			return `{"ok":true,"result":{"status":"restricted","is_member":true}}`
		default:
			return `{"ok":false,"description":"Bad Request: user not found"}`
		}
	})
	now := time.Now()
	c := checker(srv.URL, &now)

	for id, want := range map[int64]bool{1: true, 2: false, 3: true, 4: false} {
		got, err := c.IsMember(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("user %d: got %v, %v; want %v", id, got, err, want)
		}
	}

	before := calls.Load()
	if ok, _ := c.IsMember(context.Background(), 1); !ok || calls.Load() != before {
		t.Fatal("positive answer must come from cache")
	}

	now = now.Add(2 * time.Minute) // negative TTL passed, positive not yet
	c.IsMember(context.Background(), 1)
	c.IsMember(context.Background(), 2)
	if calls.Load() != before+1 {
		t.Fatalf("only the negative entry should be refetched, calls=%d", calls.Load()-before)
	}
}

func TestStaleAnswerOnOutage(t *testing.T) {
	var calls atomic.Int32
	srv := fakeTelegram(t, &calls, func(string) string { return `{"ok":true,"result":{"status":"member"}}` })
	now := time.Now()
	c := checker(srv.URL, &now)

	if ok, err := c.IsMember(context.Background(), 1); !ok || err != nil {
		t.Fatalf("warm-up: %v %v", ok, err)
	}
	srv.Close()
	now = now.Add(time.Hour)

	if ok, err := c.IsMember(context.Background(), 1); !ok || err != nil {
		t.Fatalf("want stale positive answer during outage, got %v %v", ok, err)
	}
	if _, err := c.IsMember(context.Background(), 2); err == nil {
		t.Fatal("unknown user during outage must be an error")
	}
}

func TestErrorsDoNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused
	now := time.Now()

	_, err := checker(srv.URL, &now).IsMember(context.Background(), 1)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks bot token: %v", err)
	}
}

func TestChatNotFoundIsAnError(t *testing.T) {
	var calls atomic.Int32
	srv := fakeTelegram(t, &calls, func(string) string { return `{"ok":false,"description":"Bad Request: chat not found"}` })
	now := time.Now()
	if _, err := checker(srv.URL, &now).IsMember(context.Background(), 1); err == nil {
		t.Fatal("misconfigured chat must surface as an error, not as 'not a member'")
	}
}
