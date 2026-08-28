"""No-look-ahead properties of the split construction.

A random split on time-series data is the single most productive source of
research results that do not survive contact with production. These tests
assert the two properties that stop it: strict chronological ordering, and an
embargo wide enough that a training row's label window cannot reach into the
next partition.
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from ml.features import dataset, schema


@pytest.fixture(scope="module")
def ts(synthetic_csv):
    return dataset.load(synthetic_csv, horizon="60m").timestamps()


def test_partitions_are_ordered_and_disjoint(ts):
    s = dataset.chronological_split(ts, "60m")
    assert s.train.size and s.valid.size and s.test.size
    assert not (set(s.train) & set(s.valid))
    assert not (set(s.valid) & set(s.test))
    assert ts[s.train].max() < ts[s.valid].min() < ts[s.test].min()


def test_embargo_is_at_least_two_horizons(ts):
    """Each row's label reads prices up to ts+horizon; the gap must exceed that."""
    s = dataset.chronological_split(ts, "60m")
    horizon = np.timedelta64(schema.horizon_minutes("60m"), "m")

    train_gap = ts[s.valid].min() - ts[s.train].max()
    valid_gap = ts[s.test].min() - ts[s.valid].max()
    assert train_gap > 2 * horizon
    assert valid_gap > 2 * horizon
    # The label window of the last training row closes strictly before the
    # first validation row exists. That is the property, stated directly.
    assert ts[s.train].max() + horizon < ts[s.valid].min()
    assert ts[s.valid].max() + horizon < ts[s.test].min()


def test_a_short_embargo_is_refused(ts):
    with pytest.raises(ValueError) as err:
        dataset.chronological_split(ts, "60m", embargo_multiple=1.0)
    assert "below the required 2x horizon" in str(err.value)


def test_a_longer_horizon_embargoes_more_rows(ts):
    short = dataset.chronological_split(ts, "15m")
    long = dataset.chronological_split(ts, "60m")
    assert long.embargoed > short.embargoed
    assert long.embargo_minutes == 120
    assert short.embargo_minutes == 30


def test_split_refuses_unsorted_input(ts):
    """dataset.load guarantees sorted rows; losing that must be loud."""
    shuffled = np.array(ts, copy=True)
    shuffled[[0, -1]] = shuffled[[-1, 0]]
    with pytest.raises(ValueError) as err:
        dataset.chronological_split(shuffled, "60m")
    assert "already sorted" in str(err.value)


def test_a_whole_timestamp_stays_in_one_partition(ts):
    """~100 symbols share each bar close; splitting them across partitions would
    put a symbol's peers in the future relative to it."""
    s = dataset.chronological_split(ts, "60m")
    for a, b in ((s.train, s.valid), (s.valid, s.test), (s.train, s.test)):
        assert not (set(pd.unique(ts[a])) & set(pd.unique(ts[b])))


def test_walk_forward_folds_move_forward_and_keep_the_embargo(ts):
    folds = dataset.walk_forward_folds(ts, "60m", n_folds=4)
    assert len(folds) >= 2
    horizon = np.timedelta64(schema.horizon_minutes("60m"), "m")
    last_test_start = None
    for f in folds:
        assert ts[f.train].max() < ts[f.valid].min() <= ts[f.valid].max() < ts[f.test].min()
        assert ts[f.valid].max() + horizon < ts[f.test].min()
        assert ts[f.train].max() + horizon < ts[f.valid].min()
        start = ts[f.test].min()
        if last_test_start is not None:
            assert start > last_test_start, "test windows must advance in time"
        last_test_start = start


def test_anchored_folds_grow_and_rolling_folds_do_not(ts):
    anchored = dataset.walk_forward_folds(ts, "60m", n_folds=4, anchored=True)
    rolling = dataset.walk_forward_folds(ts, "60m", n_folds=4, anchored=False)
    sizes = [f.train.size for f in anchored]
    assert sizes == sorted(sizes) and sizes[-1] > sizes[0]
    assert max(f.train.size for f in rolling) <= max(sizes)
