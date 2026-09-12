package main

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type SweepResult struct {
	SL           float64
	TP           float64
	TotalProfit  float64
	Trades       int
	WinRate      float64
	ProfitFactor float64
	Sharpe       float64
	NiftyProfit  float64
	SensexProfit float64
}

var (
	reProfit  = regexp.MustCompile(`Total Net Profit:\s+₹([-\d.]+)`)
	reTrades  = regexp.MustCompile(`Total Trades:\s+(\d+)`)
	reWinRate = regexp.MustCompile(`Win Rate:\s+([-\d.]+)%`)
	rePF      = regexp.MustCompile(`Profit Factor:\s+([-\d.]+)`)
	reSharpe  = regexp.MustCompile(`Sharpe \(per-trade\):\s+([-\d.]+|n/a)`)
	reNifty   = regexp.MustCompile(`(?i)NIFTY 50:\s+([-\d.]+)`)
	reSensex  = regexp.MustCompile(`(?i)SENSEX:\s+([-\d.]+)`)
)

func runBacktest(sl, tp float64) (SweepResult, error) {
	args := []string{
		"-strategy", "emacross",
		"-csv", "indices.csv",
		"-timeframe", "5minute",
		"-cost-bps", "0",
		"-asset", "options",
		"-product", "MIS",
		"-ema-sl", fmt.Sprintf("%.2f", sl),
		"-ema-tp", fmt.Sprintf("%.2f", tp),
	}

	cmd := exec.Command("./backtest.exe", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return SweepResult{}, fmt.Errorf("cmd failed: %v", err)
	}

	output := out.String()

	res := SweepResult{SL: sl, TP: tp}

	if m := reProfit.FindStringSubmatch(output); len(m) > 1 {
		res.TotalProfit, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reTrades.FindStringSubmatch(output); len(m) > 1 {
		res.Trades, _ = strconv.Atoi(m[1])
	}
	if m := reWinRate.FindStringSubmatch(output); len(m) > 1 {
		res.WinRate, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := rePF.FindStringSubmatch(output); len(m) > 1 {
		res.ProfitFactor, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reSharpe.FindStringSubmatch(output); len(m) > 1 && m[1] != "n/a" {
		res.Sharpe, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reNifty.FindStringSubmatch(output); len(m) > 1 {
		res.NiftyProfit, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reSensex.FindStringSubmatch(output); len(m) > 1 {
		res.SensexProfit, _ = strconv.ParseFloat(m[1], 64)
	}

	return res, nil
}

func main() {
	slValues := []float64{1.0, 1.5, 2.0, 2.5, 3.0}
	tpValues := []float64{3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 10.0, 999.0} // 999 = no TP, pure opposite EMA exit

	fmt.Printf("Starting EMA Cross SL/TP Sweep over %d combinations...\n", len(slValues)*len(tpValues))
	start := time.Now()

	var results []SweepResult
	idx := 0
	total := len(slValues) * len(tpValues)

	for _, sl := range slValues {
		for _, tp := range tpValues {
			idx++
			tpStr := fmt.Sprintf("%.1fx", tp)
			if tp >= 900 {
				tpStr = "None"
			}
			fmt.Printf("[%2d/%2d] Testing SL=%.1fxATR TP=%s ... ", idx, total, sl, tpStr)
			res, err := runBacktest(sl, tp)
			if err != nil {
				log.Printf("ERROR: %v", err)
				continue
			}
			fmt.Printf("PnL: ₹%+9.2f | PF: %.2f | WinRate: %5.1f%%\n", res.TotalProfit, res.ProfitFactor, res.WinRate)
			results = append(results, res)
		}
	}

	// Sort by Total Profit descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].TotalProfit > results[j].TotalProfit
	})

	fmt.Printf("\n=== SWEEP COMPLETED in %v ===\n", time.Since(start).Round(time.Second))
	fmt.Println("\nRANKED BY TOTAL PROFIT (SPOT INDICES: NIFTY + SENSEX):")
	fmt.Println(strings.Repeat("-", 92))
	fmt.Printf("%-4s | %-6s | %-6s | %-7s | %-8s | %-6s | %-7s | %-12s | %-12s | %-12s\n",
		"Rank", "SL", "TP", "Trades", "WinRate", "PF", "Sharpe", "NIFTY PnL", "SENSEX PnL", "Total PnL")
	fmt.Println(strings.Repeat("-", 92))

	for i, r := range results {
		tpStr := fmt.Sprintf("%.1f", r.TP)
		if r.TP >= 900 {
			tpStr = "None"
		}
		fmt.Printf("%-4d | %4.1fx | %-6s | %-7d | %6.2f%% | %6.2f | %+7.3f | ₹%+10.2f | ₹%+10.2f | ₹%+10.2f\n",
			i+1, r.SL, tpStr, r.Trades, r.WinRate, r.ProfitFactor, r.Sharpe, r.NiftyProfit, r.SensexProfit, r.TotalProfit)
	}
	fmt.Println(strings.Repeat("-", 92))
}
