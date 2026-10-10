"""Tests for scripts/build_membership.py — the pure interval logic only.

The Wikipedia fetch is not exercised here: it needs the network and the page
changes. The functions under test are the ones that turn two tables into the
membership CSV the Go loader reads.
"""

import datetime as dt
import importlib.util
import io
import pathlib
import sys

import pytest

_SCRIPT = pathlib.Path(__file__).resolve().parents[2] / "scripts" / "build_membership.py"
_spec = importlib.util.spec_from_file_location("build_membership", _SCRIPT)
bm = importlib.util.module_from_spec(_spec)
sys.modules["build_membership"] = bm  # dataclasses resolve annotations via sys.modules
_spec.loader.exec_module(bm)

FLOOR = dt.date(1990, 1, 1)


def test_parse_date_forms():
    assert bm.parse_date("2020-12-21") == dt.date(2020, 12, 21)
    assert bm.parse_date("December 21, 2020[3]") == dt.date(2020, 12, 21)
    assert bm.parse_date("1982-11-30 (1982-11-30)") == dt.date(1982, 11, 30)
    assert bm.parse_date("1997") == dt.date(1997, 1, 1)
    assert bm.parse_date("") is None
    assert bm.parse_date(None) is None


def test_normalise_matches_go():
    assert bm.normalise_ticker("brk-b") == "BRK.B"
    assert bm.normalise_ticker(" BF/B ") == "BF.B"


def test_build_rows_current_and_removed():
    current = [
        {"ticker": "AAPL", "company": "Apple", "sector": "Technology", "date_added": "1982-11-30"},
        {"ticker": "TSLA", "company": "Tesla", "sector": "Consumer Discretionary", "date_added": "December 21, 2020"},
        {"ticker": "NEW", "company": "New Co", "sector": "Industrials", "date_added": ""},
    ]
    changes = [
        {"date": "December 21, 2020", "added": "TSLA", "removed": "AIV", "removed_name": "Apartment Inv"},
        {"date": "2018-06-26", "added": "WBA", "removed": "GE", "removed_name": "General Electric"},
        {"date": "2010-01-01", "added": "GE", "removed": "", "removed_name": ""},
        {"date": "2024-01-01", "added": "GE", "removed": "WBA", "removed_name": "Walgreens"},
    ]
    rows = bm.build_rows(current, changes, FLOOR)
    by = {}
    for r in rows:
        by.setdefault(r.ticker, []).append(r)

    # Current constituents: open interval from date added.
    assert by["AAPL"][0].start == dt.date(1982, 11, 30) and by["AAPL"][0].end is None
    assert by["TSLA"][0].start == dt.date(2020, 12, 21)
    # Missing date falls back to the floor and is flagged, never silently.
    assert by["NEW"][0].start == FLOOR and "floor" in by["NEW"][0].note

    # Removal closes an interval; the start is the earlier addition when known.
    ge = by["GE"]
    assert ge[0].start == dt.date(2010, 1, 1) and ge[0].end == dt.date(2018, 6, 26) and ge[0].note == ""
    # Removal with no known addition: floor start, flagged.
    aiv = by["AIV"][0]
    assert aiv.start == FLOOR and aiv.end == dt.date(2020, 12, 21) and "unknown" in aiv.note
    # WBA added 2018, removed 2024: closed interval with a known start.
    wba = by["WBA"][0]
    assert wba.start == dt.date(2018, 6, 26) and wba.end == dt.date(2024, 1, 1)
    # GE re-added in 2024 is not a current constituent in this fixture, so it
    # has no open interval; that is the fixture's choice, not a bug.
    assert len(ge) == 1


def test_write_csv_has_go_loader_columns():
    rows = [bm.Row("AAPL", dt.date(1982, 11, 30), None, "Apple", "Technology", "")]
    buf = io.StringIO()
    bm.write_csv(rows, buf)
    lines = buf.getvalue().strip().splitlines()
    assert lines[0].startswith("ticker,start,end,company,sector")
    assert lines[1] == "AAPL,1982-11-30,,Apple,Technology,"


def test_build_rows_rejects_inverted_interval():
    # An addition recorded *after* the removal cannot open the removed interval.
    changes = [
        {"date": "2015-01-01", "added": "", "removed": "X", "removed_name": ""},
        {"date": "2016-01-01", "added": "X", "removed": "", "removed_name": ""},
    ]
    rows = bm.build_rows([], changes, FLOOR)
    assert len(rows) == 1 and rows[0].start == FLOOR and rows[0].end == dt.date(2015, 1, 1)


if __name__ == "__main__":
    pytest.main([__file__, "-q"])
