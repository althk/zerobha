package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestTelegramDeliversInOrderAndDrainsOnClose(t *testing.T) {
	var mu sync.Mutex
	var got []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body["text"])
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if body["chat_id"] != "42" {
			t.Errorf("chat_id = %q", body["chat_id"])
		}
	}))
	defer srv.Close()

	tg, err := newTelegram("TOKEN", "42", srv.URL, rate.Inf)
	if err != nil {
		t.Fatal(err)
	}
	tg.Notify(Info, "one")
	tg.Notify(Warn, "two")
	tg.Notify(Alert, "three")
	tg.Close(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"one", "⚠️ two", "🚨 three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	if paths[0] != "/botTOKEN/sendMessage" {
		t.Fatalf("path = %q", paths[0])
	}
}

func TestTelegramRedactsTokenFromErrors(t *testing.T) {
	tg := &Telegram{token: "SECRET", chatID: "1", baseURL: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}
	err := tg.send("x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitPrefersLineBreaksAndKeepsEverything(t *testing.T) {
	msg := strings.Repeat("abcdefghi\n", 10) // 100 bytes
	parts := split(msg, 35)
	if strings.Join(parts, "\n") != msg {
		t.Fatalf("content changed: %q", parts)
	}
	for _, p := range parts {
		if len(p) > 35 {
			t.Fatalf("part too long: %d", len(p))
		}
	}
	// No line break: hard cut, never inside a multi-byte rune.
	for _, p := range split(strings.Repeat("₹", 20), 10) {
		if !strings.HasPrefix(p, "₹") || len(p)%3 != 0 {
			t.Fatalf("rune split: %q", p)
		}
	}
}

func TestNewTelegramNeedsTokenAndChat(t *testing.T) {
	if _, err := NewTelegram("", "1"); err == nil {
		t.Fatal("want error for empty token")
	}
	if _, err := NewTelegram("t", ""); err == nil {
		t.Fatal("want error for empty chat id")
	}
}
