package kiteauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Kite's web login host.
const DefaultBaseURL = "https://kite.zerodha.com"

// Credentials are the account's own login, not the Kite Connect app keys.
type Credentials struct {
	UserID     string
	Password   string
	TOTPSecret string // base32 key shown when the authenticator app was set up
}

// Validate reports a missing field or a TOTP secret that cannot be decoded,
// so a typo fails at start-up instead of at the login.
func (c Credentials) Validate() error {
	if c.UserID == "" || c.Password == "" || c.TOTPSecret == "" {
		return errors.New("kite user id, password and totp secret are all required")
	}
	_, err := decodeSecret(c.TOTPSecret)
	return err
}

// Client plays the Kite web login. The zero value is not usable; use New.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
}

// New returns a Client against Kite's real login host.
func New() *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 20 * time.Second},
		Now:     time.Now,
	}
}

type apiResponse struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// RequestToken logs in with creds and returns the request token for the Kite
// Connect app behind loginURL (kiteconnect.Client.GetLoginURL()). The app must
// already have been authorised once by hand in a browser.
func (c *Client) RequestToken(ctx context.Context, creds Credentials, loginURL string) (string, error) {
	if err := creds.Validate(); err != nil {
		return "", err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	hc := *c.HTTP
	hc.Jar = jar

	// 1. Password.
	var login struct {
		RequestID string `json:"request_id"`
		TwoFAType string `json:"twofa_type"`
	}
	if err := c.post(ctx, &hc, "/api/login", url.Values{
		"user_id":  {creds.UserID},
		"password": {creds.Password},
	}, &login); err != nil {
		return "", fmt.Errorf("kite login: %w", err)
	}
	if login.RequestID == "" {
		return "", errors.New("kite login: no request_id in response")
	}
	if login.TwoFAType != "" && login.TwoFAType != "totp" {
		return "", fmt.Errorf("kite login: account 2FA is %q, not totp", login.TwoFAType)
	}

	// 2. TOTP. A code read in the last seconds of its window can expire in
	// flight, so wait for the next one instead.
	now := c.Now()
	if left := totpStep - time.Duration(now.Unix()%int64(totpStep/time.Second))*time.Second; left < 3*time.Second {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(left):
		}
		now = now.Add(left)
	}
	code, err := TOTP(creds.TOTPSecret, now)
	if err != nil {
		return "", err
	}
	if err := c.post(ctx, &hc, "/api/twofa", url.Values{
		"user_id":     {creds.UserID},
		"request_id":  {login.RequestID},
		"twofa_value": {code},
		"twofa_type":  {"totp"},
		"skip_totp":   {"true"},
	}, nil); err != nil {
		return "", fmt.Errorf("kite 2fa: %w", err)
	}

	// 3. The Connect login, now with a session cookie. Kite redirects through
	// to the app's redirect URL carrying request_token; stop there rather
	// than requesting the redirect URL itself, which nothing is serving.
	var token string
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if t := req.URL.Query().Get("request_token"); t != "" {
			token = t
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Kite-Version", "3")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("kite connect login: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if token == "" {
		return "", fmt.Errorf("kite connect login: no request_token (ended at %s, status %d) - authorise the app once by hand in a browser",
			resp.Request.URL.Redacted(), resp.StatusCode)
	}
	return token, nil
}

func (c *Client) post(ctx context.Context, hc *http.Client, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Kite-Version", "3")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("status %d, unreadable response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || r.Status != "success" {
		return fmt.Errorf("status %d: %s", resp.StatusCode, r.Message)
	}
	if out != nil && len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}
