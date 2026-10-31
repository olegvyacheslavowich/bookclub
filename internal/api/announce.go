package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"bookclub/internal/store"
	"bookclub/internal/tg"
)

// Announcer posts to the club's Telegram group. tg.Announcer implements it.
type Announcer interface {
	Announce(ctx context.Context, m tg.Message) error
}

const (
	announceTimeout = 4 * time.Second
	// tgTextLimit is Telegram's limit for a message, counted here in UTF-16 units to be safe with emoji.
	tgTextLimit = 4096
)

// announce posts m to the group. A failure is logged and never fails the member's change.
//
// It runs synchronously, before the response is written: Yandex Serverless Containers may freeze
// an instance as soon as the response is sent, so a goroutine started afterwards might never run.
// WithoutCancel: the change is already saved, so the post goes out even if the member closes the app.
func (s *Server) announce(r *http.Request, what string, m tg.Message) {
	if s.announcer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), announceTimeout)
	defer cancel()
	if err := s.announcer.Announce(ctx, m); err != nil {
		s.log.Warn("group announcement failed", "what", what, "err", err)
	}
}

// Messages avoid gendered verbs ("добавил/добавила"): Telegram does not tell us a member's gender.

func (s *Server) newBookMessage(b store.Book) tg.Message {
	var sb strings.Builder
	sb.WriteString("📚 Новая книга\n")
	sb.WriteString(bookLine(b))
	sb.WriteString("\nПредлагает: ")
	sb.WriteString(b.AddedBy.Name)
	if b.Note != "" {
		sb.WriteString("\n\n")
		sb.WriteString(b.Note)
	}
	return s.withOpenButton(fitText(sb.String(), tgTextLimit), b.ID)
}

func (s *Server) newReviewMessage(b store.Book, r store.Review) tg.Message {
	var sb strings.Builder
	sb.WriteString("✍️ Отзыв на ")
	sb.WriteString(bookLine(b))
	sb.WriteString("\n")
	sb.WriteString(r.Author.Name)
	sb.WriteString(": ")
	sb.WriteString(starLine(r.Rating))
	if r.Text != "" {
		sb.WriteString("\n\n")
		sb.WriteString(r.Text)
	}
	// The review text comes last, so trimming an over-long message only shortens the review.
	return s.withOpenButton(fitText(sb.String(), tgTextLimit), b.ID)
}

// bookLine: «Обломов» — Гончаров
func bookLine(b store.Book) string {
	line := "«" + b.Title + "»"
	if b.Author != "" {
		line += " — " + b.Author
	}
	return line
}

// starLine: ★★★★☆ 4 из 5
func starLine(rating int) string {
	rating = min(max(rating, 0), 5)
	return strings.Repeat("★", rating) + strings.Repeat("☆", 5-rating) + " " + strconv.Itoa(rating) + " из 5"
}

// withOpenButton adds a button that opens the Mini App straight on the book. Inline web_app buttons
// don't work in groups, so it is a plain link to https://t.me/<bot>/<app>?startapp=book_<id>;
// the front end reads start_param and navigates to the book.
func (s *Server) withOpenButton(text, bookID string) tg.Message {
	m := tg.Message{Text: text}
	if link := s.bookAppLink(bookID); link != "" {
		m.ButtonText, m.ButtonURL = "Открыть в приложении", link
	}
	return m
}

// fitText cuts s to at most limit UTF-16 units, ending with "…" when it had to cut.
func fitText(s string, limit int) string {
	if utf16Len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	budget := limit - utf16Len(ellipsis)
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n+w > budget {
			return strings.TrimRight(s[:i], " \n") + ellipsis
		}
		n += w
	}
	return s
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if w := utf16.RuneLen(r); w > 0 {
			n += w
		} else {
			n++
		}
	}
	return n
}
