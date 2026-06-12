// Package db provides the persistence layer for storing application state.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
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
		// Completed round-trip trades (entry+exit), reconciled from broker fills.
		// trade_key is "<exit_order_id>:<lot_index>" so one exit order closing
		// multiple entry lots produces multiple idempotent rows.
		`CREATE TABLE IF NOT EXISTS trades (
			trade_key TEXT PRIMARY KEY,
			symbol TEXT,
			strategy TEXT,
			direction TEXT,
			quantity REAL,
			entry_price REAL,
			exit_price REAL,
			pnl REAL,
			entry_time DATETIME,
			exit_time DATETIME,
			exit_reason TEXT
		);`,
		// Periodic intraday equity snapshots for the dashboard PnL curve.
		`CREATE TABLE IF NOT EXISTS equity_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME,
			balance REAL,
			realized_pnl REAL,
			unrealized_pnl REAL,
			open_positions INTEGER
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

// --- Trade Methods ---

// SaveTrade persists a completed round-trip trade. key must be unique per
// matched lot (e.g. "<exit_order_id>:<lot_index>") so re-running the
// reconciler over the same broker fills is idempotent.
func (s *Store) SaveTrade(key string, t models.Trade) error {
	query := `INSERT OR IGNORE INTO trades
			  (trade_key, symbol, strategy, direction, quantity, entry_price, exit_price, pnl, entry_time, exit_time, exit_reason)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`

	qty, _ := t.Quantity.Float64()
	entry, _ := t.EntryPrice.Float64()
	exit, _ := t.ExitPrice.Float64()
	pnl, _ := t.PnL.Float64()

	_, err := s.db.Exec(query, key, t.Symbol, t.Strategy, t.Direction, qty, entry, exit, pnl, t.EntryTime, t.ExitTime, t.ExitReason)
	return err
}

// GetTradeHistory returns completed trades exited after `since`,
// in chronological order (oldest first).
func (s *Store) GetTradeHistory(since time.Time) ([]models.Trade, error) {
	query := `SELECT symbol, strategy, direction, quantity, entry_price, exit_price, pnl, entry_time, exit_time, exit_reason
			  FROM trades WHERE exit_time >= ? ORDER BY exit_time ASC;`

	rows, err := s.db.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trades []models.Trade
	for rows.Next() {
		var t models.Trade
		var qty, entry, exit, pnl float64
		if err := rows.Scan(&t.Symbol, &t.Strategy, &t.Direction, &qty, &entry, &exit, &pnl, &t.EntryTime, &t.ExitTime, &t.ExitReason); err != nil {
			return nil, err
		}
		t.Quantity = decimal.NewFromFloat(qty)
		t.EntryPrice = decimal.NewFromFloat(entry)
		t.ExitPrice = decimal.NewFromFloat(exit)
		t.PnL = decimal.NewFromFloat(pnl)
		trades = append(trades, t)
	}
	return trades, rows.Err()
}

// --- Equity Snapshot Methods ---

// EquityPoint is one periodic sample of account state used for the
// intraday PnL curve on the dashboard.
type EquityPoint struct {
	Timestamp     time.Time `json:"timestamp"`
	Balance       float64   `json:"balance"`
	RealizedPnL   float64   `json:"realized_pnl"`
	UnrealizedPnL float64   `json:"unrealized_pnl"`
	OpenPositions int       `json:"open_positions"`
}

func (s *Store) SaveEquitySnapshot(p EquityPoint) error {
	query := `INSERT INTO equity_snapshots (timestamp, balance, realized_pnl, unrealized_pnl, open_positions)
			  VALUES (?, ?, ?, ?, ?);`
	_, err := s.db.Exec(query, p.Timestamp, p.Balance, p.RealizedPnL, p.UnrealizedPnL, p.OpenPositions)
	return err
}

// GetEquitySnapshots returns snapshots taken after `since`, oldest first.
func (s *Store) GetEquitySnapshots(since time.Time) ([]EquityPoint, error) {
	query := `SELECT timestamp, balance, realized_pnl, unrealized_pnl, open_positions
			  FROM equity_snapshots WHERE timestamp >= ? ORDER BY timestamp ASC;`

	rows, err := s.db.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []EquityPoint
	for rows.Next() {
		var p EquityPoint
		if err := rows.Scan(&p.Timestamp, &p.Balance, &p.RealizedPnL, &p.UnrealizedPnL, &p.OpenPositions); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
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
