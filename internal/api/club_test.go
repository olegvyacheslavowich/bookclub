package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"bookclub/internal/store"
)

const clubChat = "-8800555"

// clubServer: "group" opened the app from the club's group, "forwarded" from some other chat,
// "menu" without a chat at all (e.g. from the list of recent apps).
func clubServer(t *testing.T, members MemberChecker) *harness {
	t.Helper()
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		Store: st,
		Auth: fakeAuth{
			"group":        {ID: 10, FirstName: "Вера", ChatInstance: clubChat, ChatType: "supergroup"},
			"forwarded":    {ID: 11, FirstName: "Гость", ChatInstance: "-1234", ChatType: "private"},
			"menu":         {ID: 12, FirstName: "Нина"},
			"group-broken": {ID: 500, FirstName: "Сбой", ChatInstance: clubChat},
			"menu-broken":  {ID: 500, FirstName: "Сбой"},
			"menu-member":  {ID: 1, FirstName: "Анна"},
		},
		Members:           members,
		ClubChatInstances: []string{clubChat},
		Logger:            discardLogger(),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}
}

func TestClubChatInstanceOnly(t *testing.T) {
	h := clubServer(t, nil) // CLUB_CHAT_INSTANCE without CLUB_CHAT_ID: no Bot API at all
	for who, want := range map[string]int{
		"group":     http.StatusOK,
		"forwarded": http.StatusForbidden,
		"menu":      http.StatusForbidden,
	} {
		if got := h.do(who, "GET", "/api/me", "", nil); got != want {
			t.Errorf("%s: got %d, want %d", who, got, want)
		}
	}
}

func TestClubChatInstanceWithMemberCheck(t *testing.T) {
	h := clubServer(t, fakeMembers{1: true})
	for who, want := range map[string]int{
		"group":        http.StatusOK,                 // from the group: no getChatMember needed
		"group-broken": http.StatusOK,                 // ...even while Telegram is unreachable
		"menu-member":  http.StatusOK,                 // elsewhere: getChatMember says yes
		"forwarded":    http.StatusForbidden,          // elsewhere: getChatMember says no
		"menu-broken":  http.StatusServiceUnavailable, // elsewhere and Telegram is unreachable
	} {
		if got := h.do(who, "GET", "/api/me", "", nil); got != want {
			t.Errorf("%s: got %d, want %d", who, got, want)
		}
	}
}

func TestEmptyChatInstanceNeverMatches(t *testing.T) {
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A misconfigured empty entry must not let in everyone who opened the app without a chat.
	s := New(Config{
		Store:             st,
		Auth:              fakeAuth{"menu": {ID: 12, FirstName: "Нина"}},
		ClubChatInstances: []string{""},
		Logger:            discardLogger(),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	h := &harness{t: t, srv: srv}
	if got := h.do("menu", "GET", "/api/me", "", nil); got != http.StatusForbidden {
		t.Fatalf("got %d", got)
	}
}
