"""Summarise a backtester trade dump in per-trade basis points.

The rupee totals the backtester prints are not the measure of an index signal:
the sizer puts more capital behind some trades than others, so a positive rupee
total can sit on top of a negative average return (CLAUDE.md records exactly
that trap for gapfade). What matters for a signal destined for option execution
is the average per-trade move in bps of the entry price, and whether it is
distinguishable from zero.

Usage:
    python scripts/analyze_trades.py sr_trades.csv [--split 2025-09-01]
"""

import argparse
import csv
import math
from collections import Counter, defaultdict


def load(path):
    rows = []
    with open(path, newline="", encoding="utf-8") as fh:
        for r in csv.DictReader(fh):
            entry = float(r["entry_price"])
            if entry <= 0:
                continue
            pnl = float(r["pnl_gross"])
            qty = float(r["quantity"])
            if qty <= 0:
                continue
            # Return per unit, as a fraction of the entry price: the same trade
            # is worth the same bps whether the sizer bought one unit or ten.
            rows.append(
                {
                    "symbol": r["symbol"],
                    "direction": r["direction"],
                    "date": r["entry_time"][:10],
                    "bps": (pnl / qty) / entry * 10_000,
                    "pnl": pnl,
                    "reason": r["exit_reason"],
                }
            )
    return rows


def stats(rows):
    n = len(rows)
    if n == 0:
        return None
    bps = [r["bps"] for r in rows]
    mean = sum(bps) / n
    if n > 1:
        var = sum((b - mean) ** 2 for b in bps) / (n - 1)
        se = math.sqrt(var / n)
    else:
        se = 0.0
    t = mean / se if se > 0 else 0.0
    wins = [b for b in bps if b > 0]
    gross_win = sum(wins)
    gross_loss = -sum(b for b in bps if b <= 0)
    pf = gross_win / gross_loss if gross_loss > 0 else float("inf")
    return {
        "n": n,
        "bps": mean,
        "t": t,
        "win": 100.0 * len(wins) / n,
        "pf": pf,
        "rupees": sum(r["pnl"] for r in rows),
    }


def line(label, s):
    if s is None:
        print(f"{label:<28} {'—':>8}")
        return
    print(
        f"{label:<28} {s['n']:>6}  {s['bps']:>8.2f} bps  t={s['t']:>6.2f}  "
        f"win={s['win']:>5.1f}%  PF={s['pf']:>5.2f}  Rs{s['rupees']:>10,.0f}"
    )


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("path")
    ap.add_argument(
        "--split",
        default="2025-09-01",
        help="first date of the out-of-sample window (default 2025-09-01)",
    )
    args = ap.parse_args()

    rows = load(args.path)
    if not rows:
        print("no trades")
        return

    print(f"{'':<28} {'n':>6}  {'mean':>12}  {'t':>8}  {'win':>10}  {'PF':>8}  {'net':>12}")
    line("ALL", stats(rows))
    line(f"IS  (< {args.split})", stats([r for r in rows if r["date"] < args.split]))
    line(f"OOS (>= {args.split})", stats([r for r in rows if r["date"] >= args.split]))

    by_symbol = defaultdict(list)
    for r in rows:
        by_symbol[r["symbol"]].append(r)
    print()
    for sym in sorted(by_symbol):
        line(sym, stats(by_symbol[sym]))
        line(f"  {sym} OOS", stats([r for r in by_symbol[sym] if r["date"] >= args.split]))

    print()
    for side in ("LONG", "SHORT"):
        line(side, stats([r for r in rows if r["direction"] == side]))

    print("\nexit reasons:")
    for reason, count in Counter(r["reason"] for r in rows).most_common():
        sub = stats([r for r in rows if r["reason"] == reason])
        print(f"  {reason:<16} {count:>5}  {sub['bps']:>8.2f} bps")

    days = len({(r["symbol"], r["date"]) for r in rows})
    print(f"\n{len(rows)} trades over {days} symbol-days")


if __name__ == "__main__":
    main()
