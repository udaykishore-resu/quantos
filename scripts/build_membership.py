#!/usr/bin/env python3
"""Build a point-in-time S&P 500 membership table for QuantOS backtests.

Usage:

    python3 scripts/build_membership.py --out data/membership/sp500.csv
    python3 scripts/build_membership.py --html /path/to/saved_page.html --out ...

The table has the columns the Go loader expects (internal/universe/membership.go):

    ticker,start,end,company,sector

Where the data comes from
-------------------------
The public Wikipedia page "List of S&P 500 companies" carries two tables: the
current constituents with their "Date added", and a "Selected changes" table
of additions and removals going back to the 1990s. Neither is an official
record. The current table is complete; the changes table is explicitly
*selected*, and gets thinner the further back you go.

What this fixes and what it does not
------------------------------------
It fixes the pre-inclusion bias: a backtest can no longer buy NVDA in 2001 on
the strength of its 2024 index weight. Names removed from the index get a
closed interval, so a strategy cannot keep trading them after the removal
either.

It does not resurrect delisted names. A ticker removed in 2009 because the
company failed will usually have no price history at a free data source, so
it is absent from the replay directory rather than present-and-losing. The
survivorship correction is therefore partial, and the walk-forward report
says so in its notes rather than letting the reader assume otherwise.

For a removed ticker whose addition date is not in the changes table, the
interval starts at --floor (default 1990-01-01). That is a placeholder and is
flagged in the "note" column.
"""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import io
import re
import sys
from dataclasses import dataclass

WIKI_URL = "https://en.wikipedia.org/wiki/List_of_S%26P_500_companies"


@dataclass
class Row:
    ticker: str
    start: dt.date
    end: dt.date | None
    company: str = ""
    sector: str = ""
    note: str = ""


def normalise_ticker(t: str) -> str:
    """Match domain.NormalizeTicker: upper-case, '-' and '/' become '.'."""
    t = (t or "").strip().upper()
    return t.replace("-", ".").replace("/", ".")


_DATE_RE = re.compile(r"(\d{4})-(\d{2})-(\d{2})")
_MONTHS = {m: i for i, m in enumerate(
    ["January", "February", "March", "April", "May", "June", "July",
     "August", "September", "October", "November", "December"], start=1)}


def parse_date(s: str) -> dt.date | None:
    """Accept ISO dates and the 'Month D, YYYY' form Wikipedia uses.

    Footnote markers ('[12]') and trailing qualifiers are ignored. Anything
    unparseable returns None rather than a guess.
    """
    if s is None:
        return None
    s = re.sub(r"\[.*?\]", "", str(s)).strip()
    m = _DATE_RE.search(s)
    if m:
        y, mo, d = (int(x) for x in m.groups())
        return dt.date(y, mo, d)
    m = re.match(r"([A-Z][a-z]+)\s+(\d{1,2}),\s*(\d{4})", s)
    if m and m.group(1) in _MONTHS:
        return dt.date(int(m.group(3)), _MONTHS[m.group(1)], int(m.group(2)))
    m = re.match(r"(\d{4})$", s)
    if m:
        return dt.date(int(m.group(1)), 1, 1)
    return None


def build_rows(current: list[dict], changes: list[dict], floor: dt.date) -> list[Row]:
    """Combine the constituents table and the changes table into intervals.

    `current` rows: {"ticker", "company", "sector", "date_added"}
    `changes` rows: {"date", "added", "removed"} with tickers possibly empty.

    Rules, in order:
      1. Every current constituent gets [date_added, open). A missing date
         falls back to the floor and is flagged.
      2. Every removal in the changes table closes an interval at that date.
         The interval opens at the most recent *earlier* addition of the same
         ticker in the changes table, else at the floor (flagged).
      3. A ticker that was removed and later re-added gets two intervals; the
         Go loader merges overlaps.
    """
    rows: list[Row] = []
    additions: dict[str, list[dt.date]] = {}
    for ch in changes:
        d = parse_date(ch.get("date"))
        t = normalise_ticker(ch.get("added", ""))
        if d and t:
            additions.setdefault(t, []).append(d)
    for t in additions:
        additions[t].sort()

    current_tickers: set[str] = set()
    for c in current:
        t = normalise_ticker(c.get("ticker", ""))
        if not t:
            continue
        current_tickers.add(t)
        d = parse_date(c.get("date_added"))
        note = ""
        if d is None:
            d, note = floor, "date_added missing; floor used"
        rows.append(Row(t, d, None, c.get("company", ""), c.get("sector", ""), note))

    for ch in changes:
        d = parse_date(ch.get("date"))
        t = normalise_ticker(ch.get("removed", ""))
        if not (d and t):
            continue
        earlier = [a for a in additions.get(t, []) if a < d]
        if earlier:
            start, note = earlier[-1], ""
        else:
            start, note = floor, "addition date unknown; floor used"
        if start >= d:
            continue
        rows.append(Row(t, start, d, ch.get("removed_name", ""), "", note))

    rows.sort(key=lambda r: (r.ticker, r.start))
    return rows


def write_csv(rows: list[Row], out) -> None:
    w = csv.writer(out)
    w.writerow(["ticker", "start", "end", "company", "sector", "note"])
    for r in rows:
        w.writerow([r.ticker, r.start.isoformat(), r.end.isoformat() if r.end else "",
                    r.company, r.sector, r.note])


# --- Wikipedia parsing (needs pandas + lxml; only used from main) -------------

def tables_from_html(html: str) -> tuple[list[dict], list[dict]]:
    import pandas as pd  # imported lazily so the pure functions stay testable

    tables = pd.read_html(io.StringIO(html))
    current_df = None
    changes_df = None
    for t in tables:
        cols = [str(c) if not isinstance(c, tuple) else " ".join(str(x) for x in c) for c in t.columns]
        joined = " ".join(cols).lower()
        if current_df is None and "symbol" in joined and "date added" in joined:
            current_df = t
        elif changes_df is None and "added" in joined and "removed" in joined and "date" in joined:
            changes_df = t
    if current_df is None:
        raise SystemExit("could not find the constituents table (expected 'Symbol' and 'Date added' columns)")
    if changes_df is None:
        raise SystemExit("could not find the changes table (expected 'Date', 'Added', 'Removed' columns)")

    def col(df, *needles):
        for c in df.columns:
            name = c if isinstance(c, str) else " ".join(str(x) for x in c)
            if all(n in name.lower() for n in needles):
                return c
        return None

    current = []
    for _, r in current_df.iterrows():
        current.append({
            "ticker": r[col(current_df, "symbol")],
            "company": r[col(current_df, "security")] if col(current_df, "security") is not None else "",
            "sector": r[col(current_df, "gics sector")] if col(current_df, "gics sector") is not None else "",
            "date_added": r[col(current_df, "date added")],
        })
    changes = []
    c_date = col(changes_df, "date")
    c_add = col(changes_df, "added", "ticker") or col(changes_df, "added", "symbol")
    c_rem = col(changes_df, "removed", "ticker") or col(changes_df, "removed", "symbol")
    c_rem_name = col(changes_df, "removed", "security")
    for _, r in changes_df.iterrows():
        changes.append({
            "date": r[c_date],
            "added": "" if c_add is None or str(r[c_add]) == "nan" else r[c_add],
            "removed": "" if c_rem is None or str(r[c_rem]) == "nan" else r[c_rem],
            "removed_name": "" if c_rem_name is None or str(r[c_rem_name]) == "nan" else r[c_rem_name],
        })
    return current, changes


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, help="output CSV path")
    ap.add_argument("--html", help="a saved copy of the Wikipedia page, instead of fetching it")
    ap.add_argument("--floor", default="1990-01-01", help="start date for intervals with no known addition date")
    args = ap.parse_args(argv)

    floor = dt.date.fromisoformat(args.floor)
    if args.html:
        html = open(args.html, encoding="utf-8").read()
    else:
        import urllib.request
        req = urllib.request.Request(WIKI_URL, headers={"User-Agent": "quantos-membership/1.0 (research)"})
        with urllib.request.urlopen(req, timeout=60) as resp:
            html = resp.read().decode("utf-8", errors="replace")

    current, changes = tables_from_html(html)
    rows = build_rows(current, changes, floor)
    with open(args.out, "w", newline="", encoding="utf-8") as f:
        write_csv(rows, f)
    flagged = sum(1 for r in rows if r.note)
    closed = sum(1 for r in rows if r.end)
    print(f"wrote {len(rows)} intervals for {len({r.ticker for r in rows})} tickers to {args.out}")
    print(f"  {closed} closed intervals (removals), {flagged} flagged with a placeholder start")
    print("  source: Wikipedia 'List of S&P 500 companies' — the changes table is selected, not complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())
