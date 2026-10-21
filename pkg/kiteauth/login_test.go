package kiteauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA1 key "12345678901234567890", truncated to 6 digits.
func TestTOTPMatchesRFC6238(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for unix, want := range cases {
		got, err := TOTP(secret, time.Unix(unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("t=%d: got %s, want %s", unix, got, want)
		}
	}
}

func TestTOTPAcceptsPastedSecretForms(t *testing.T) {
	want, _ := TOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	for _, s := range []string{"gezd gnbv gy3t qojq gezd gnbv gy3t qojq", " GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ== "} {
		got, err := TOTP(s, time.Unix(59, 0))
		if err != nil || got != want {
			t.Errorf("%q: got %s, %v; want %s", s, got, err, want)
		}
	}
	if _, err := TOTP("not base32!", time.Now()); err == nil {
		t.Error("expected an error for an invalid secret")
	}
}

// fakeKite mimics the three steps: password, TOTP, then the Connect login
// redirecting to the app's redirect URL with a request token.
func fakeKite(t *testing.T, secret string, now time.Time) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("user_id") != "AB1234" || r.FormValue("password") != "pw" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"status":"error","message":"Invalid username or password."}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"request_id":"req1","twofa_type":"totp"}}`))
	})
	mux.HandleFunc("/api/twofa", func(w http.ResponseWriter, r *http.Request) {
		want, _ := TOTP(secret, now)
		if r.FormValue("request_id") != "req1" || r.FormValue("twofa_value") != want {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"status":"error","message":"Invalid TOTP"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "enctoken", Value: "sess", Path: "/"})
		_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
	})
	mux.HandleFunc("/connect/login", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("enctoken"); err != nil || c.Value != "sess" {
			w.WriteHeader(http.StatusOK) // the login form: not logged in
			return
		}
		http.Redirect(w, r, "/connect/finish?sess_id=x", http.StatusFound)
	})
	mux.HandleFunc("/connect/finish", func(w http.ResponseWriter, r *http.Request) {
		// The app's redirect URL, which nothing serves in the real flow.
		http.Redirect(w, r, "http://127.0.0.1:1/auth/kite/callback?request_token=tok123&action=login&status=success", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testClient(base string, now time.Time) *Client {
	c := New()
	c.BaseURL = base
	c.Now = func() time.Time { return now }
	return c
}

func TestRequestTokenFullFlow(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1_700_000_010, 0) // mid-window: no wait
	srv := fakeKite(t, secret, now)

	tok, err := testClient(srv.URL, now).RequestToken(context.Background(),
		Credentials{UserID: "AB1234", Password: "pw", TOTPSecret: secret},
		srv.URL+"/connect/login?api_key=k&v=3")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok123" {
		t.Fatalf("got token %q", tok)
	}
}

func TestRequestTokenReportsWrongPassword(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1_700_000_010, 0)
	srv := fakeKite(t, secret, now)

	_, err := testClient(srv.URL, now).RequestToken(context.Background(),
		Credentials{UserID: "AB1234", Password: "wrong", TOTPSecret: secret},
		srv.URL+"/connect/login?api_key=k&v=3")
	if err == nil || !strings.Contains(err.Error(), "Invalid username or password") {
		t.Fatalf("got %v", err)
	}
}

func TestRequestTokenReportsWrongTOTPSecret(t *testing.T) {
	now := time.Unix(1_700_000_010, 0)
	srv := fakeKite(t, "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", now)

	_, err := testClient(srv.URL, now).RequestToken(context.Background(),
		Credentials{UserID: "AB1234", Password: "pw", TOTPSecret: "JBSWY3DPEHPK3PXP"},
		srv.URL+"/connect/login?api_key=k&v=3")
	if err == nil || !strings.Contains(err.Error(), "Invalid TOTP") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRequiresAllFields(t *testing.T) {
	if err := (Credentials{UserID: "AB1234", Password: "pw"}).Validate(); err == nil {
		t.Error("missing secret accepted")
	}
	if err := (Credentials{UserID: "AB1234", Password: "pw", TOTPSecret: "!!"}).Validate(); err == nil {
		t.Error("bad secret accepted")
	}
}
