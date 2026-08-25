// Package db provides the persistence layer for storing application state.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
			is_paper INTEGER DEFAULT 0,
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
			exit_reason TEXT,
			is_paper INTEGER DEFAULT 0
		);`,
		// Periodic intraday equity snapshots for the dashboard PnL curve.
		`CREATE TABLE IF NOT EXISTS equity_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME,
			balance REAL,
			realized_pnl REAL,
			unrealized_pnl REAL,
			open_positions INTEGER,
			is_paper INTEGER DEFAULT 0
		);`,
		// Paper-broker state, one row per trading date. The paper broker holds
		// balance, positions and resting protective orders in memory, so a
		// mid-session restart would otherwise come back flat with full virtual
		// capital while the day's positions are still open. Keyed by date so a
		// stale day's state can never be restored into a new session.
		`CREATE TABLE IF NOT EXISTS paper_state (
			trade_date TEXT PRIMARY KEY,
			state TEXT NOT NULL,
			updated_at DATETIME
		);`,
	}

	for _, query := range queries {
		if _, err := s.db.Exec(query); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}
	}

	// Dynamic column migrations for existing databases
	migrations := []string{
		`ALTER TABLE orders ADD COLUMN is_paper INTEGER DEFAULT 0;`,
		`ALTER TABLE trades ADD COLUMN is_paper INTEGER DEFAULT 0;`,
		`ALTER TABLE equity_snapshots ADD COLUMN is_paper INTEGER DEFAULT 0;`,
	}
	for _, m := range migrations {
		_, _ = s.db.Exec(m) // Ignore if column already exists
	}

	return nil
}

// --- Order Methods ---

func (s *Store) SaveOrder(o models.Order, status string) error {
	query := `INSERT INTO orders (order_id, symbol, side, quantity, price, status, strategy, is_paper, timestamp)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			  ON CONFLICT(order_id) DO UPDATE SET status=excluded.status, is_paper=excluded.is_paper;`

	qty, _ := o.Quantity.Float64()
	price, _ := o.Price.Float64()
	strategy := o.Metadata["Strategy"]
	isPaper := boolToInt(o.IsPaper || strings.HasPrefix(o.ID, PaperOrderPrefix))

	_, err := s.db.Exec(query, o.ID, o.Symbol, o.Side, qty, price, status, strategy, isPaper, time.Now())
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
			  (trade_key, symbol, strategy, direction, quantity, entry_price, exit_price, pnl, entry_time, exit_time, exit_reason, is_paper)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`

	qty, _ := t.Quantity.Float64()
	entry, _ := t.EntryPrice.Float64()
	exit, _ := t.ExitPrice.Float64()
	pnl, _ := t.PnL.Float64()
	isPaper := boolToInt(t.IsPaper || strings.HasPrefix(key, PaperOrderPrefix))

	_, err := s.db.Exec(query, key, t.Symbol, t.Strategy, t.Direction, qty, entry, exit, pnl, t.EntryTime, t.ExitTime, t.ExitReason, isPaper)
	return err
}

// GetTradeHistory returns completed trades exited after `since`,
// in chronological order (oldest first).
//
// Results are scoped to one execution mode. Paper and live trades share this
// table, and blending them would contaminate every aggregate the dashboard
// derives from it — win rate, profit factor, Sharpe, drawdown, the equity
// curve — with fills that were never real. Callers pass the mode they are
// running in.
func (s *Store) GetTradeHistory(since time.Time, paper bool) ([]models.Trade, error) {
	query := `SELECT symbol, strategy, direction, quantity, entry_price, exit_price, pnl, entry_time, exit_time, exit_reason, is_paper
			  FROM trades WHERE exit_time >= ? AND is_paper = ? ORDER BY exit_time ASC;`

	rows, err := s.db.Query(query, since, boolToInt(paper))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trades []models.Trade
	for rows.Next() {
		var t models.Trade
		var qty, entry, exit, pnl float64
		var isPaper int
		if err := rows.Scan(&t.Symbol, &t.Strategy, &t.Direction, &qty, &entry, &exit, &pnl, &t.EntryTime, &t.ExitTime, &t.ExitReason, &isPaper); err != nil {
			return nil, err
		}
		t.Quantity = decimal.NewFromFloat(qty)
		t.EntryPrice = decimal.NewFromFloat(entry)
		t.ExitPrice = decimal.NewFromFloat(exit)
		t.PnL = decimal.NewFromFloat(pnl)
		t.IsPaper = (isPaper == 1)
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
	IsPaper       bool      `json:"is_paper"`
}

func (s *Store) SaveEquitySnapshot(p EquityPoint) error {
	query := `INSERT INTO equity_snapshots (timestamp, balance, realized_pnl, unrealized_pnl, open_positions, is_paper)
			  VALUES (?, ?, ?, ?, ?, ?);`
	_, err := s.db.Exec(query, p.Timestamp, p.Balance, p.RealizedPnL, p.UnrealizedPnL, p.OpenPositions, boolToInt(p.IsPaper))
	return err
}

// GetEquitySnapshots returns snapshots taken after `since`, oldest first,
// scoped to one execution mode for the same reason as GetTradeHistory: a
// virtual-capital curve and a real one are not points on the same series.
func (s *Store) GetEquitySnapshots(since time.Time, paper bool) ([]EquityPoint, error) {
	query := `SELECT timestamp, balance, realized_pnl, unrealized_pnl, open_positions, is_paper
			  FROM equity_snapshots WHERE timestamp >= ? AND is_paper = ? ORDER BY timestamp ASC;`

	rows, err := s.db.Query(query, since, boolToInt(paper))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []EquityPoint
	for rows.Next() {
		var p EquityPoint
		var isPaper int
		if err := rows.Scan(&p.Timestamp, &p.Balance, &p.RealizedPnL, &p.UnrealizedPnL, &p.OpenPositions, &isPaper); err != nil {
			return nil, err
		}
		p.IsPaper = (isPaper == 1)
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

// --- Paper Trading State ---

// PaperOrderPrefix tags every order id the paper broker mints. It is the
// last-resort mode marker: an order that reaches persistence without IsPaper
// set is still recognisable by its id, so a live row can never be created from
// a simulated fill through a path that forgot to propagate the flag.
const PaperOrderPrefix = "PAPER-"

// boolToInt converts a mode flag to the INTEGER SQLite stores it as.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SavePaperState persists the paper broker's opaque state blob for one trading
// date, replacing any previous snapshot for that date. The broker owns the
// encoding; this layer only stores bytes, so the broker's internal shape can
// change without a schema migration.
func (s *Store) SavePaperState(tradeDate string, state []byte) error {
	query := `INSERT INTO paper_state (trade_date, state, updated_at)
			  VALUES (?, ?, ?)
			  ON CONFLICT(trade_date) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at;`
	_, err := s.db.Exec(query, tradeDate, string(state), time.Now())
	return err
}

// LoadPaperState returns the blob saved for tradeDate, or nil when there is
// none. A missing row is the normal first-run case, not an error.
func (s *Store) LoadPaperState(tradeDate string) ([]byte, error) {
	var state string
	err := s.db.QueryRow(`SELECT state FROM paper_state WHERE trade_date = ?;`, tradeDate).Scan(&state)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(state), nil
}
