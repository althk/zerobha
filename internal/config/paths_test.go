package config

import (
	"path/filepath"
	"testing"
)

// The trader writes to two independent destinations, and a deployment's working
// directory is not the repository root. Defaulting both relative keeps a local
// run working; making them settable is what stops the container from writing
// its log directory into the data volume instead of the logs volume mounted
// beside it, where the backup looks for it.
func TestPathsConfig(t *testing.T) {
	t.Run("defaults are relative", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Paths.DBPath != "zerobha.db" {
			t.Errorf("db_path = %q, want the relative default", cfg.Paths.DBPath)
		}
		if cfg.Paths.LogDir != "logs" {
			t.Errorf("log_dir = %q, want the relative default", cfg.Paths.LogDir)
		}
		if filepath.IsAbs(cfg.Paths.DBPath) || filepath.IsAbs(cfg.Paths.LogDir) {
			t.Error("defaults must not be absolute — a local run writes beside the repo")
		}
	})

	t.Run("each is settable independently", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"

[paths]
log_dir = "/app/logs"
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Paths.LogDir != "/app/logs" {
			t.Errorf("log_dir = %q, want /app/logs", cfg.Paths.LogDir)
		}
		// Setting one must leave the other at its default: the whole point is
		// that the database and the logs go to different volumes.
		if cfg.Paths.DBPath != "zerobha.db" {
			t.Errorf("db_path = %q, want it left at the default", cfg.Paths.DBPath)
		}
	})

	t.Run("both settable together", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfig(t, `
api_key = "k"
api_secret = "s"

[paths]
db_path = "/app/data/zerobha.db"
log_dir = "/app/logs"
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Paths.DBPath != "/app/data/zerobha.db" || cfg.Paths.LogDir != "/app/logs" {
			t.Errorf("got db=%q log=%q, want both honoured", cfg.Paths.DBPath, cfg.Paths.LogDir)
		}
	})
}
