// Package tg checks club membership through the Telegram Bot API.
package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemberChecker answers "is this user in the club's Telegram group?" via getChatMember.
// The bot must be a member of the group. Answers are cached: a positive one for 10 minutes,
// a negative one for 1 minute, so someone who has just joined is not locked out for long.
type MemberChecker struct {
	token   string
	chatID  string
	baseURL string
	client  *http.Client
	posTTL  time.Duration
	negTTL  time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[int64]cacheEntry
}

type cacheEntry struct {
	member  bool
	expires time.Time
}

func NewMemberChecker(botToken, chatID string) *MemberChecker {
	return &MemberChecker{
		token:   botToken,
		chatID:  chatID,
		baseURL: "https://api.telegram.org",
		client:  &http.Client{Timeout: 5 * time.Second},
		posTTL:  10 * time.Minute,
		negTTL:  time.Minute,
		now:     time.Now,
		cache:   make(map[int64]cacheEntry),
	}
}

func (c *MemberChecker) IsMember(ctx context.Context, userID int64) (bool, error) {
	c.mu.Lock()
	cached, ok := c.cache[userID]
	c.mu.Unlock()
	if ok && c.now().Before(cached.expires) {
		return cached.member, nil
	}

	member, err := c.fetch(ctx, userID)
	if err != nil {
		if ok {
			// Telegram is unreachable: a stale answer is better than locking everyone out.
			return cached.member, nil
		}
		return false, err
	}

	ttl := c.negTTL
	if member {
		ttl = c.posTTL
	}
	c.mu.Lock()
	c.cache[userID] = cacheEntry{member: member, expires: c.now().Add(ttl)}
	c.mu.Unlock()
	return member, nil
}

func (c *MemberChecker) fetch(ctx context.Context, userID int64) (bool, error) {
	q := url.Values{"chat_id": {c.chatID}, "user_id": {strconv.FormatInt(userID, 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/bot"+c.token+"/getChatMember?"+q.Encode(), nil)
	if err != nil {
		return false, errors.New("getChatMember: cannot build request") // the parse error would echo the token
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// *url.Error prints the full URL, and the URL contains the bot token. Keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return false, fmt.Errorf("getChatMember: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Status   string `json:"status"`
			IsMember bool   `json:"is_member"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return false, fmt.Errorf("getChatMember: decode response (HTTP %d): %w", resp.StatusCode, err)
	}

	if !body.OK {
		desc := strings.ToLower(body.Description)
		// Accounts that never were in the chat come back as an error rather than status "left".
		if strings.Contains(desc, "user not found") || strings.Contains(desc, "member not found") {
			return false, nil
		}
		return false, fmt.Errorf("getChatMember: telegram error (HTTP %d): %s", resp.StatusCode, body.Description)
	}

	switch body.Result.Status {
	case "creator", "administrator", "member":
		return true, nil
	case "restricted":
		return body.Result.IsMember, nil
	default: // "left", "kicked"
		return false, nil
	}
}
