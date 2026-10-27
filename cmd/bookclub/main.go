// Command bookclub serves the book club Telegram Mini App: the static front end and its JSON API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bookclub/internal/api"
	"bookclub/internal/auth"
	"bookclub/internal/store"
	"bookclub/internal/tg"
	"bookclub/web"
)

type config struct {
	addr       string
	dataFile   string
	botToken   string
	chatID     string
	adminIDs   []int64
	devMode    bool
	initMaxAge time.Duration
}

func loadConfig() (config, error) {
	c := config{
		addr:       env("ADDR", ":8080"),
		dataFile:   env("DATA_FILE", "data/bookclub.json"),
		botToken:   os.Getenv("BOT_TOKEN"),
		chatID:     os.Getenv("CLUB_CHAT_ID"),
		devMode:    os.Getenv("DEV_MODE") == "1",
		initMaxAge: 24 * time.Hour,
	}

	if c.botToken == "" && !c.devMode {
		return c, errors.New("BOT_TOKEN is required (or DEV_MODE=1 for local testing)")
	}
	if c.chatID != "" && c.botToken == "" {
		return c, errors.New("CLUB_CHAT_ID needs BOT_TOKEN to check membership")
	}

	for _, part := range strings.Split(os.Getenv("ADMIN_IDS"), ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return c, fmt.Errorf("ADMIN_IDS: %q is not a Telegram user ID", part)
		}
		c.adminIDs = append(c.adminIDs, id)
	}

	if v := os.Getenv("INITDATA_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("INITDATA_MAX_AGE: %w", err)
		}
		c.initMaxAge = d
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.dataFile), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.OpenFile(cfg.dataFile)
	if err != nil {
		return err
	}

	var members api.MemberChecker
	if cfg.chatID != "" {
		members = tg.NewMemberChecker(cfg.botToken, cfg.chatID)
	} else {
		log.Warn("CLUB_CHAT_ID is not set: anyone who opens the Mini App gets access")
	}
	if len(cfg.adminIDs) == 0 {
		log.Warn("ADMIN_IDS is empty: nobody can manage meetings; user IDs appear in the log as 'user seen'")
	}
	if cfg.devMode {
		log.Warn("DEV_MODE=1: requests can impersonate any user. Never run like this in production")
	}

	srv := api.New(api.Config{
		Store:    st,
		Auth:     auth.NewValidator(cfg.botToken, cfg.initMaxAge),
		Members:  members,
		AdminIDs: cfg.adminIDs,
		DevMode:  cfg.devMode,
		Static:   web.FS(),
		Logger:   log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "data", cfg.dataFile, "admins", len(cfg.adminIDs))
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
