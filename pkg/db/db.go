// Package db provides the persistence layer for storing application state.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"zerobha/internal/models"

	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

type Store struct {
	db *sql.DB
}

func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	store := &Store{db: db}
	if err := store.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to init schema: %w", err)
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) initSchema() error {
	// Orders Table
	queries := []string{
		`CREATE TABLE IF NOT EXISTS orders (
			order_id TEXT PRIMARY KEY,
			symbol TEXT,
			side TEXT,
			quantity REAL,
			price REAL,
			status TEXT,
			strategy TEXT,
			timestamp DATETIME
		);`,
		// Signals Table
		`CREATE TABLE IF NOT EXISTS signals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			symbol TEXT,
			strategy TEXT,
			type TEXT,
			price REAL,
			stop_loss REAL,
			target REAL,
			timestamp DATETIME
		);`,
		// KV Store for Strategy State
		`CREATE TABLE IF NOT EXISTS kv_store (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME
		);`,
	}

	for _, query := range queries {
		if _, err := s.db.Exec(query); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}
	}
	return nil
}

// --- Order Methods ---

func (s *Store) SaveOrder(o models.Order, status string) error {
	query := `INSERT INTO orders (order_id, symbol, side, quantity, price, status, strategy, timestamp)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			  ON CONFLICT(order_id) DO UPDATE SET status=excluded.status;`

	qty, _ := o.Quantity.Float64()
	price, _ := o.Price.Float64()
	strategy := o.Metadata["Strategy"]

	_, err := s.db.Exec(query, o.ID, o.Symbol, o.Side, qty, price, status, strategy, time.Now())
	return err
}

func (s *Store) GetOrderStrategy(symbol string) (string, error) {
	query := `SELECT strategy FROM orders WHERE symbol = ? AND strategy IS NOT NULL AND strategy != '' ORDER BY timestamp DESC LIMIT 1;`
	var strategy string
	err := s.db.QueryRow(query, symbol).Scan(&strategy)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return strategy, err
}

// --- Signal Methods ---

func (s *Store) SaveSignal(sig *models.Signal) error {
	query := `INSERT INTO signals (symbol, strategy, type, price, stop_loss, target, timestamp)
			  VALUES (?, ?, ?, ?, ?, ?, ?);`

	price, _ := sig.Price.Float64()
	sl, _ := sig.StopLoss.Float64()
	tgt, _ := sig.Target.Float64()
	strategy := sig.Metadata["Strategy"]

	_, err := s.db.Exec(query, sig.Symbol, strategy, sig.Type.String(), price, sl, tgt, time.Now())
	return err
}

// --- KV Store Methods (for Strategy State) ---

func (s *Store) SetState(key string, value interface{}) error {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return err
	}

	query := `INSERT INTO kv_store (key, value, updated_at)
			  VALUES (?, ?, ?)
			  ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at;`

	_, err = s.db.Exec(query, key, string(jsonBytes), time.Now())
	return err
}

func (s *Store) GetState(key string, dest interface{}) error {
	var valueStr string
	query := `SELECT value FROM kv_store WHERE key = ?;`

	err := s.db.QueryRow(query, key).Scan(&valueStr)
	if err == sql.ErrNoRows {
		return nil // Not found is not an error for us, just empty
	}
	if err != nil {
		return err
	}

	return json.Unmarshal([]byte(valueStr), dest)
}
