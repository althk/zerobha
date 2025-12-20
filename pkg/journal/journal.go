// Package journal provides a thread-safe CSV logger for trading signals and orders.
package journal

import (
	"encoding/csv"
	"fmt"
	"os"
	"sync"
	"time"

	"zerobha/internal/models"
)

// Journal handles thread-safe CSV logging
type Journal struct {
	mu       sync.Mutex
	file     *os.File
	writer   *csv.Writer
	filepath string
}

// NewJournal creates a new Journal instance
func NewJournal(filepath string) (*Journal, error) {
	// Check if file exists to determine if we need to write header
	_, err := os.Stat(filepath)
	fileExists := !os.IsNotExist(err)

	f, err := os.OpenFile(filepath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open journal file: %v", err)
	}

	j := &Journal{
		file:     f,
		writer:   csv.NewWriter(f),
		filepath: filepath,
	}

	if !fileExists {
		// Write Header
		header := []string{
			"Timestamp",
			"EventType",
			"Symbol",
			"Action",
			"Price",
			"Quantity",
			"StopLoss",
			"Target",
			"Status",
			"Details",
		}
		if err := j.writer.Write(header); err != nil {
			return nil, fmt.Errorf("failed to write header: %v", err)
		}
		j.writer.Flush()
	}

	return j, nil
}

// LogSignal logs a trading signal
func (j *Journal) LogSignal(signal *models.Signal) {
	j.mu.Lock()
	defer j.mu.Unlock()

	record := []string{
		time.Now().Format(time.RFC3339),
		"SIGNAL",
		signal.Symbol,
		signal.Type.String(),
		signal.Price.StringFixed(2),
		"0", // No quantity yet
		signal.StopLoss.StringFixed(2),
		signal.Target.StringFixed(2),
		"GENERATED",
		fmt.Sprintf("Metadata: %v", signal.Metadata),
	}

	_ = j.writer.Write(record)
	j.writer.Flush()
}

// LogOrder logs an order execution attempt
func (j *Journal) LogOrder(order models.Order, status string, details string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	record := []string{
		time.Now().Format(time.RFC3339),
		"ORDER",
		order.Symbol,
		order.Side.String(),
		order.Price.StringFixed(2),
		order.Quantity.String(),
		order.StopLoss.StringFixed(2),
		order.Target.StringFixed(2),
		status,
		details,
	}

	_ = j.writer.Write(record)
	j.writer.Flush()
}

// LogRiskBlock logs when risk manager blocks a trade
func (j *Journal) LogRiskBlock(signal *models.Signal, reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	record := []string{
		time.Now().Format(time.RFC3339),
		"RISK_BLOCK",
		signal.Symbol,
		signal.Type.String(),
		signal.Price.StringFixed(2),
		"0",
		signal.StopLoss.StringFixed(2),
		signal.Target.StringFixed(2),
		"BLOCKED",
		reason,
	}

	_ = j.writer.Write(record)
	j.writer.Flush()
}

// Close closes the file handle
func (j *Journal) Close() error {
	j.writer.Flush()
	return j.file.Close()
}
