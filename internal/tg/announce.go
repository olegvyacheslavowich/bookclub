package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Message is a plain-text post to the club's group, optionally with one link button under it.
type Message struct {
	Text       string
	ButtonText string // the button is shown only when both ButtonText and ButtonURL are set
	ButtonURL  string
}

// Announcer posts messages to the club's group through sendMessage.
// The bot must be in the group and allowed to send messages there.
type Announcer struct {
	token   string
	chatID  string
	baseURL string
	client  *http.Client
}

// NewAnnouncer: baseURL "" means DefaultAPIURL.
func NewAnnouncer(baseURL, botToken, chatID string) *Announcer {
	return &Announcer{
		token:   botToken,
		chatID:  chatID,
		baseURL: apiURL(baseURL),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type sendMessageRequest struct {
	ChatID      string `json:"chat_id"`
	Text        string `json:"text"` // no parse_mode: book titles with * or < must not break markup
	LinkPreview struct {
		IsDisabled bool `json:"is_disabled"`
	} `json:"link_preview_options"`
	ReplyMarkup *struct {
		InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
	} `json:"reply_markup,omitempty"`
}

func (a *Announcer) Announce(ctx context.Context, m Message) error {
	req := sendMessageRequest{ChatID: a.chatID, Text: m.Text}
	req.LinkPreview.IsDisabled = true
	if m.ButtonText != "" && m.ButtonURL != "" {
		req.ReplyMarkup = &struct {
			InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
		}{InlineKeyboard: [][]inlineButton{{{Text: m.ButtonText, URL: m.ButtonURL}}}}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("sendMessage: encode: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/bot"+a.token+"/sendMessage", bytes.NewReader(payload))
	if err != nil {
		return errors.New("sendMessage: cannot build request") // the parse error would echo the token
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// *url.Error prints the full URL, and the URL contains the bot token. Keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("sendMessage: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("sendMessage: decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if !body.OK {
		// Typical: 403 "bot is not a member of the group chat", 400 "not enough rights to send text messages".
		return fmt.Errorf("sendMessage: telegram error (HTTP %d): %s", resp.StatusCode, body.Description)
	}
	return nil
}
