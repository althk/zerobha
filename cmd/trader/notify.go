package main

import (
	"bytes"
	"log"
	"os"
	"time"

	"zerobha/internal/config"
	"zerobha/pkg/notify"
)

// buildNotifier returns the Telegram notifier when [telegram] is configured,
// otherwise one that sends nothing. The returned close drains the queue and
// is safe to call more than once.
func buildNotifier(cfg *config.Config) (notify.Notifier, func()) {
	if !cfg.Telegram.Enabled() {
		log.Println("Telegram: not configured ([telegram] token/chat_id), notifications go to the log only")
		return notify.Discard{}, func() {}
	}
	tg, err := notify.NewTelegram(cfg.Telegram.Token, cfg.Telegram.ChatID)
	if err != nil {
		log.Printf("WARNING: Telegram disabled: %v", err)
		return notify.Discard{}, func() {}
	}
	log.Printf("Telegram: notifications on (chat %s)", cfg.Telegram.ChatID)
	return tg, func() { tg.Close(5 * time.Second) }
}

// startupMessage is the start-up notification: host, then the effective config
// exactly as -show-config prints it (credentials reported as set/missing only).
func startupMessage(cfg *config.Config, configPath string, paper bool) string {
	var b bytes.Buffer
	host, _ := os.Hostname()
	b.WriteString("▶️ zerobha starting")
	if host != "" {
		b.WriteString(" on " + host)
	}
	b.WriteString("\n\n")
	printConfig(&b, cfg, configPath, paper)
	return b.String()
}
