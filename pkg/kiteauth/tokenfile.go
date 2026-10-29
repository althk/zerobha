package kiteauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TokenFileName is the shared token file, kept in the user's home directory
// so every app on the machine that uses the same Kite app key can reuse the
// day's token instead of logging in again. A fresh login invalidates the
// previous token, so sharing it is what lets two apps run on one key.
const TokenFileName = ".kite_token"

// Token is the file's content. IssuedAt decides freshness: Kite resets every
// access token at 06:00 IST.
type Token struct {
	APIKey      string    `json:"api_key"`
	AccessToken string    `json:"access_token"`
	IssuedAt    time.Time `json:"issued_at"`
}

// ErrNoToken means there is no usable token: the file is missing, was written
// for another app key, or holds a token issued before the last reset.
var ErrNoToken = errors.New("no fresh kite token on file")

var ist = time.FixedZone("IST", 5*3600+1800)

// DefaultTokenPath is ~/.kite_token.
func DefaultTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, TokenFileName), nil
}

// Fresh reports whether the token is still valid at now, i.e. no 06:00 IST
// reset lies between IssuedAt and now.
func (t Token) Fresh(now time.Time) bool {
	if t.AccessToken == "" || t.IssuedAt.IsZero() {
		return false
	}
	at := t.IssuedAt.In(ist)
	reset := time.Date(at.Year(), at.Month(), at.Day(), 6, 0, 0, 0, ist)
	if !reset.After(at) {
		reset = reset.AddDate(0, 0, 1)
	}
	return now.Before(reset)
}

// SaveToken writes the token readable by the owner only. It writes a
// temporary file and renames it, so a reader never sees half a file.
func SaveToken(path string, t Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return fmt.Errorf("write kite token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write kite token: %w", err)
	}
	return nil
}

// LoadToken returns the token on file if it belongs to apiKey and is fresh at
// now, else ErrNoToken.
func LoadToken(path, apiKey string, now time.Time) (Token, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, ErrNoToken
	}
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, fmt.Errorf("read kite token %s: %w", path, err)
	}
	if t.APIKey != apiKey || !t.Fresh(now) {
		return Token{}, ErrNoToken
	}
	return t, nil
}
