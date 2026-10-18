package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	telegramAPI = "https://api.telegram.org"
	// Telegram rejects messages over 4096 characters; leave room for the
	// level prefix.
	maxMessageLen = 4000
	queueSize     = 256
)

// Telegram posts to one chat.
//
// Sends are queued and delivered by a single goroutine at about one message a
// second (Telegram's per-chat limit), so a slow or rate-limited API can never
// stall the trading loop. The queue is bounded and drops the oldest message on
// overflow: the newest describes the current state.
type Telegram struct {
	token   string
	chatID  string
	baseURL string
	client  *http.Client
	limiter *rate.Limiter

	mu      sync.Mutex
	pending []string
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// NewTelegram starts a notifier posting to chatID with the bot token.
func NewTelegram(token, chatID string) (*Telegram, error) {
	return newTelegram(token, chatID, telegramAPI, rate.Every(time.Second))
}

func newTelegram(token, chatID, baseURL string, r rate.Limit) (*Telegram, error) {
	if token == "" || chatID == "" {
		return nil, fmt.Errorf("notify: Telegram needs a bot token and a chat id")
	}
	t := &Telegram{
		token:   token,
		chatID:  chatID,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 10 * time.Second},
		limiter: rate.NewLimiter(r, 1),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go t.run()
	return t, nil
}

// Notify queues a message and returns at once. Long messages are split.
func (t *Telegram) Notify(level Level, msg string) {
	switch level {
	case Warn:
		msg = "⚠️ " + msg
	case Alert:
		msg = "🚨 " + msg
	}
	t.mu.Lock()
	for _, part := range split(msg, maxMessageLen) {
		if len(t.pending) >= queueSize {
			t.pending = t.pending[1:]
			log.Printf("notify: Telegram queue full, dropped oldest message")
		}
		t.pending = append(t.pending, part)
	}
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Close delivers what is queued, waiting at most timeout. Safe to call twice.
func (t *Telegram) Close(timeout time.Duration) {
	t.once.Do(func() { close(t.stop) })
	select {
	case <-t.done:
	case <-time.After(timeout):
		log.Printf("notify: Telegram closed with messages undelivered after %v", timeout)
	}
}

func (t *Telegram) run() {
	defer close(t.done)
	for {
		t.drain()
		select {
		case <-t.wake:
		case <-t.stop:
			t.drain()
			return
		}
	}
}

func (t *Telegram) drain() {
	for {
		t.mu.Lock()
		if len(t.pending) == 0 {
			t.mu.Unlock()
			return
		}
		msg := t.pending[0]
		t.pending = t.pending[1:]
		t.mu.Unlock()

		_ = t.limiter.Wait(context.Background())
		if err := t.send(msg); err != nil {
			log.Printf("notify: Telegram send failed: %v", err)
		}
	}
}

func (t *Telegram) send(text string) error {
	body, err := json.Marshal(map[string]string{"chat_id": t.chatID, "text": text})
	if err != nil {
		return err
	}
	url := t.baseURL + "/bot" + t.token + "/sendMessage"
	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		// net/http errors quote the URL, and the URL carries the token.
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), t.token, "<redacted>"))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned HTTP %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// split cuts msg into pieces of at most n bytes, preferring line breaks.
func split(msg string, n int) []string {
	var parts []string
	for len(msg) > n {
		cut := strings.LastIndex(msg[:n], "\n")
		if cut <= 0 {
			cut = n
			// Do not cut a multi-byte rune in half.
			for cut > 0 && !isRuneStart(msg[cut]) {
				cut--
			}
		}
		parts = append(parts, msg[:cut])
		msg = strings.TrimPrefix(msg[cut:], "\n")
	}
	return append(parts, msg)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

var (
	_ Notifier = Discard{}
	_ Notifier = (*Telegram)(nil)
)
