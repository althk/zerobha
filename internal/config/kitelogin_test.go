package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKiteConfig(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	body := "api_key = \"k\"\napi_secret = \"s\"\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKiteLoginKeysMustBeSetTogether(t *testing.T) {
	_, err := LoadConfig(writeKiteConfig(t, "kite_user_id = \"AB1234\"\n"))
	if err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("got %v", err)
	}
}

func TestKiteLoginRejectsBadTOTPSecret(t *testing.T) {
	_, err := LoadConfig(writeKiteConfig(t, "kite_user_id = \"AB1234\"\nkite_password = \"pw\"\nkite_totp_secret = \"!!\"\n"))
	if err == nil {
		t.Fatal("bad secret accepted")
	}
}

func TestKiteLoginFromConfigAndEnv(t *testing.T) {
	cfg, err := LoadConfig(writeKiteConfig(t, ""))
	if err != nil || cfg.AutoLogin() {
		t.Fatalf("no keys: AutoLogin=%v err=%v", cfg != nil && cfg.AutoLogin(), err)
	}
	t.Setenv("KITE_USER_ID", "AB1234")
	t.Setenv("KITE_PASSWORD", "pw")
	t.Setenv("KITE_TOTP_SECRET", "JBSWY3DPEHPK3PXP")
	cfg, err = LoadConfig(writeKiteConfig(t, ""))
	if err != nil || !cfg.AutoLogin() || cfg.KitePassword != "pw" {
		t.Fatalf("env keys not applied: %+v %v", cfg, err)
	}
}
