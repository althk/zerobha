#!/usr/bin/env python3
"""
fetch_1m_data.py
Downloads 1-minute historical candles from Upstox for NIFTY 50 constituent stocks
and writes them in the Zerobha backtest CSV format:
test/data/1m/<symbol>_real.csv
"""

import os
import sys
import csv
import json
import time
import urllib.request
import urllib.error
from datetime import datetime
from concurrent.futures import ThreadPoolExecutor, as_completed

MONTH_CHUNKS = [
    ("2026-06-01", "2026-06-30"),
    ("2026-07-01", "2026-07-31"),
    ("2026-08-01", "2026-08-31"),
    ("2026-09-01", "2026-09-10"),
]

USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

def fetch_chunk(isin, from_date, to_date, retries=3):
    key = f"NSE_EQ|{isin}"
    url = f"https://api.upstox.com/v2/historical-candle/{key}/1minute/{to_date}/{from_date}"
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": "application/json"})

    for attempt in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                data = json.loads(resp.read().decode())
                candles = data.get("data", {}).get("candles", [])
                return candles
        except urllib.error.HTTPError as e:
            if e.code == 429:
                time.sleep(1.0 * (attempt + 1))
                continue
            if e.code == 404 or e.code == 400:
                return []
            if attempt == retries - 1:
                print(f"[{isin}] HTTP {e.code} for {from_date}..{to_date}: {e}")
                return []
            time.sleep(0.5)
        except Exception as e:
            if attempt == retries - 1:
                print(f"[{isin}] Error for {from_date}..{to_date}: {e}")
                return []
            time.sleep(0.5)
    return []

def download_symbol(symbol, isin, out_dirs):
    stem = symbol.lower().replace(" ", "")
    all_candles = []
    seen = set()

    for from_date, to_date in MONTH_CHUNKS:
        candles = fetch_chunk(isin, from_date, to_date)
        for c in candles:
            if len(c) < 6:
                continue
            ts = c[0]
            if ts not in seen:
                seen.add(ts)
                all_candles.append(c)
        time.sleep(0.1)

    if not all_candles:
        print(f"[-] {symbol}: No candles returned")
        return symbol, 0

    # Sort chronologically (oldest first)
    all_candles.sort(key=lambda x: x[0])

    # Write to target dirs
    rows = [["timestamp", "open", "high", "low", "close", "volume"]]
    for c in all_candles:
        ts, o, h, l, cl, vol = c[0], c[1], c[2], c[3], c[4], c[5]
        rows.append([ts, f"{float(o):.2f}", f"{float(h):.2f}", f"{float(l):.2f}", f"{float(cl):.2f}", str(int(vol))])

    for out_dir in out_dirs:
        os.makedirs(out_dir, exist_ok=True)
        out_path = os.path.join(out_dir, f"{stem}_real.csv")
        with open(out_path, "w", newline="") as f:
            writer = csv.writer(f)
            writer.writerows(rows)

    print(f"[+] {symbol}: Saved {len(all_candles)} candles (span: {all_candles[0][0][:10]} to {all_candles[-1][0][:10]})")
    return symbol, len(all_candles)

def main():
    csv_file = "ind_nifty50list.csv"
    if not os.path.exists(csv_file):
        print(f"CSV file {csv_file} not found")
        sys.exit(1)

    stocks = []
    with open(csv_file, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        for row in reader:
            sym = row.get("Symbol") or row.get("symbol")
            isin = row.get("ISIN Code") or row.get("isin")
            if sym and isin:
                stocks.append((sym.strip(), isin.strip()))

    print(f"Found {len(stocks)} stocks in {csv_file}")

    out_dirs = [
        os.path.join("test", "data", "1m"),
        os.path.join("test", "data", "1minute"),
    ]

    total_downloaded = 0
    start_time = time.time()

    with ThreadPoolExecutor(max_workers=4) as executor:
        futures = {executor.submit(download_symbol, sym, isin, out_dirs): sym for sym, isin in stocks}
        for future in as_completed(futures):
            sym, count = future.result()
            total_downloaded += count

    elapsed = time.time() - start_time
    print(f"\nFinished in {elapsed:.1f}s. Downloaded {total_downloaded} total 1m candles for {len(stocks)} stocks.")

if __name__ == "__main__":
    main()
