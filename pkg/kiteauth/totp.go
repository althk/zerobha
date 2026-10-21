// Package kiteauth logs in to Kite Connect without a browser: it plays the
// web login (user id + password, then a TOTP code) and returns the request
// token Kite would otherwise hand to the redirect URL.
package kiteauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// totpStep is the RFC 6238 time step Kite's authenticator codes use.
const totpStep = 30 * time.Second

// decodeSecret accepts the base32 key Kite shows when 2FA is set up, in the
// forms people paste it: lower case, spaced in groups, with or without padding.
func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("totp secret is not valid base32: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("totp secret is empty")
	}
	return key, nil
}

// TOTP returns the 6-digit RFC 6238 code (HMAC-SHA1, 30 s step) for t.
func TOTP(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(t.Unix())/uint64(totpStep/time.Second), 6), nil
}

func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}
