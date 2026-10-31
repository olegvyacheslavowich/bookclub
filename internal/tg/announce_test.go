package tg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnnounce(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+token+"/sendMessage" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	a := NewAnnouncer("", token, "-100500")
	a.baseURL = srv.URL
	err := a.Announce(context.Background(), Message{Text: "«Обломов» *не markdown*", ButtonText: "Открыть", ButtonURL: "https://t.me/bot/club?startapp=book_ab"})
	if err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "-100500" || got["text"] != "«Обломов» *не markdown*" {
		t.Fatalf("payload: %v", got)
	}
	if _, ok := got["parse_mode"]; ok {
		t.Fatal("text must be sent without parse_mode")
	}
	kb := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
	if kb["text"] != "Открыть" || kb["url"] != "https://t.me/bot/club?startapp=book_ab" {
		t.Fatalf("button: %v", kb)
	}

	got = nil
	if err := a.Announce(context.Background(), Message{Text: "без кнопки"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reply_markup"]; ok {
		t.Fatal("no button expected")
	}
}

func TestAnnounceErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"ok":false,"description":"Forbidden: bot is not a member of the group chat"}`)
	}))
	a := NewAnnouncer("", token, "-100500")
	a.baseURL = srv.URL
	err := a.Announce(context.Background(), Message{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("want telegram description, got %v", err)
	}

	srv.Close() // now the request fails at the network level, where the URL would be printed
	err = a.Announce(context.Background(), Message{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must not contain the token: %v", err)
	}
}

func TestAPIURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                          DefaultAPIURL,
		"https://tg.example.com/x/": "https://tg.example.com/x",
		"https://tg.example.com/x":  "https://tg.example.com/x",
	} {
		if got := apiURL(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
