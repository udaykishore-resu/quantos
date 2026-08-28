"""Loading, validating and chronologically splitting the exported feature CSV.

Every guard in this file exists because the corresponding mistake is silent.
A reordered column, a label band that does not match the one stamped on the
artifact, or a shuffled train/test split all produce a model that trains
cleanly, scores well offline and is wrong in production. So the loader is
strict and the splitter refuses to be random.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import pandas as pd

from ml.features import schema


@dataclass(frozen=True)
class Dataset:
    """A validated feature table plus the metadata a trainer needs to be honest."""

    frame: pd.DataFrame
    horizon: str
    #: Half-width of the FLAT class in basis points, inferred from the data and
    #: cross-checked against the declared value. Stamped onto the artifact.
    flat_band_bps: float
    #: sha256 of the source CSV bytes. Part of the artifact version, so two
    #: artifacts trained on different data can never share a version string.
    data_digest: str
    source: Path

    @property
    def n(self) -> int:
        return len(self.frame)

    def matrix(self) -> np.ndarray:
        """Return X in ModelFeatureSet order, as float64."""
        return self.frame[schema.MODEL_FEATURE_SET].to_numpy(dtype=np.float64, copy=True)

    def labels(self) -> np.ndarray:
        """Return y as class indices into schema.ALL_OUTCOMES."""
        col = self.frame[f"label_{self.horizon}"]
        lookup = {name: i for i, name in enumerate(schema.ALL_OUTCOMES)}
        unknown = sorted(set(col.unique()) - set(lookup))
        if unknown:
            raise ValueError(
                f"label_{self.horizon} contains outcomes the Go domain does not "
                f"define: {unknown}"
            )
        return col.map(lookup).to_numpy(dtype=np.int64)

    def forward_returns(self) -> np.ndarray:
        return self.frame[f"fwd_ret_bps_{self.horizon}"].to_numpy(dtype=np.float64)

    def timestamps(self) -> np.ndarray:
        return self.frame["ts"].to_numpy()

    def regimes(self) -> np.ndarray:
        return self.frame["regime"].to_numpy()


def file_digest(path: Path) -> str:
    """sha256 of a file, streamed so a multi-hundred-megabyte CSV is affordable."""
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load(
    path: str | Path,
    horizon: str = "60m",
    declared_flat_band_bps: float = 15.0,
) -> Dataset:
    """Load the exported CSV, asserting it matches the Go feature contract.

    The column check is exact and positional, not a set membership test. Two
    features whose names were swapped in the exporter would pass a set check and
    silently transpose two coefficient columns.
    """
    path = Path(path)
    if not path.is_file():
        raise FileNotFoundError(
            f"{path} does not exist; run `make export-features` to produce it"
        )

    frame = pd.read_csv(path)
    actual = list(frame.columns)

    # Which horizons the file actually carries. The exporter writes every
    # domain.DefaultHorizons pair, but accepting a narrower file lets a test
    # fixture stay small without weakening the feature-block check.
    present = [h for h in schema.DEFAULT_HORIZONS if f"label_{h}" in actual]
    expected = schema.expected_csv_header(present)
    if actual != expected:
        raise ValueError(_header_diff(expected, actual, path))

    if horizon not in present:
        raise ValueError(
            f"{path} has no columns for horizon {horizon!r}; it carries {present}"
        )

    feature_block = frame[schema.MODEL_FEATURE_SET]
    if not np.isfinite(feature_block.to_numpy(dtype=np.float64)).all():
        bad = feature_block.columns[
            ~np.isfinite(feature_block.to_numpy(dtype=np.float64)).all(axis=0)
        ].tolist()
        raise ValueError(
            f"{path}: non-finite values in {bad}; the Go exporter only writes warm "
            "features, so this means the file is corrupt or hand-edited"
        )

    frame = frame.copy()
    frame["ts"] = pd.to_datetime(frame["ts"], utc=True, format="ISO8601")
    # Stable chronological order with a deterministic tie-break, so two runs over
    # the same file see rows in the same order regardless of pandas internals.
    frame = frame.sort_values(["ts", "ticker"], kind="mergesort").reset_index(drop=True)

    band = _infer_flat_band(frame, horizon, declared_flat_band_bps, path)

    return Dataset(
        frame=frame,
        horizon=horizon,
        flat_band_bps=band,
        data_digest=file_digest(path),
        source=path,
    )


def _header_diff(expected: list[str], actual: list[str], path: Path) -> str:
    lines = [
        f"{path}: CSV header does not match domain.ModelFeatureSet.",
        "This is the training/serving skew guard: refusing to train rather than",
        "fit a model on a feature layout mlinfer will not serve.",
        f"  expected ({len(expected)}): {expected}",
        f"  actual   ({len(actual)}): {actual}",
    ]
    missing = [c for c in expected if c not in actual]
    extra = [c for c in actual if c not in expected]
    if missing:
        lines.append(f"  missing: {missing}")
    if extra:
        lines.append(f"  unexpected: {extra}")
    if not missing and not extra:
        first = next(
            (i for i, (e, a) in enumerate(zip(expected, actual)) if e != a), None
        )
        lines.append(
            f"  same names, wrong order: position {first} is {actual[first]!r}, "
            f"expected {expected[first]!r}"
        )
    return "\n".join(lines)


def _infer_flat_band(
    frame: pd.DataFrame, horizon: str, declared: float, path: Path
) -> float:
    """Recover the FLAT band the exporter used, and check it against `declared`.

    The band is the whole meaning of the classes. Taking it on trust from a flag
    is how a model ends up stamped with a band it was not trained on, at which
    point "UP: 0.63" is a claim about a threshold nobody can name.
    """
    ret = frame[f"fwd_ret_bps_{horizon}"].to_numpy(dtype=np.float64)
    lab = frame[f"label_{horizon}"].to_numpy()
    flat = np.abs(ret[lab == "FLAT"])
    directional = np.abs(ret[lab != "FLAT"])
    if flat.size == 0 or directional.size == 0:
        raise ValueError(
            f"{path}: horizon {horizon} has no {'FLAT' if flat.size == 0 else 'directional'} "
            "rows, so the class definition cannot be recovered"
        )
    lo, hi = float(flat.max()), float(directional.min())
    if not lo <= declared <= hi:
        raise ValueError(
            f"{path}: labels for {horizon} are inconsistent with the declared flat "
            f"band of {declared} bps. The data implies a band in [{lo:.4f}, {hi:.4f}]. "
            "Re-export with a matching -flat-band-bps or pass the right value."
        )
    return float(declared)


# --- splitting --------------------------------------------------------------


@dataclass(frozen=True)
class Split:
    """Row index arrays for one chronological train/valid/test partition."""

    train: np.ndarray
    valid: np.ndarray
    test: np.ndarray
    embargo_minutes: int
    #: Rows dropped into the embargo gaps. Reported rather than hidden: a large
    #: number here means the split boundaries fell somewhere expensive.
    embargoed: int

    def bounds(self, ts: np.ndarray) -> dict[str, tuple[pd.Timestamp, pd.Timestamp]]:
        out = {}
        for name, idx in (("train", self.train), ("valid", self.valid), ("test", self.test)):
            if idx.size == 0:
                continue
            out[name] = (pd.Timestamp(ts[idx].min()), pd.Timestamp(ts[idx].max()))
        return out


def chronological_split(
    ts: np.ndarray,
    horizon: str,
    train_frac: float = 0.6,
    valid_frac: float = 0.2,
    embargo_multiple: float = 2.0,
) -> Split:
    """Split rows by time, with an embargo gap between every pair of partitions.

    Two properties matter and neither survives a random split.

    The first is ordering: a validation row must never precede a training row,
    or the model is being scored on its own past.

    The second is the embargo. Each row's label is built from prices up to
    ``ts + horizon``, so a training row within one horizon of the validation
    boundary carries information from inside the validation window. The gap is
    ``embargo_multiple`` horizons wide — at least two, never less than one —
    and the rows inside it are discarded rather than assigned to either side.

    The boundaries are snapped to timestamp edges so that the ~100 symbols that
    share a bar close are never split across partitions.
    """
    if not 0 < train_frac < 1 or not 0 < valid_frac < 1 or train_frac + valid_frac >= 1:
        raise ValueError(
            f"train_frac={train_frac} and valid_frac={valid_frac} must be positive "
            "and leave room for a test partition"
        )
    if embargo_multiple < 2.0:
        raise ValueError(
            f"embargo_multiple={embargo_multiple} is below the required 2x horizon; "
            "a shorter gap leaks the label window across the boundary"
        )

    ts = pd.to_datetime(pd.Series(ts), utc=True).to_numpy()
    order = np.argsort(ts, kind="stable")
    if not np.array_equal(order, np.arange(len(ts))):
        raise ValueError(
            "chronological_split expects rows already sorted by timestamp; "
            "dataset.load guarantees that, so an unsorted array means the caller "
            "reindexed and lost the ordering"
        )

    embargo = np.timedelta64(
        int(round(schema.horizon_minutes(horizon) * embargo_multiple)), "m"
    )

    uniq = np.unique(ts)
    if uniq.size < 3:
        raise ValueError(
            f"only {uniq.size} distinct timestamps; a chronological split with an "
            "embargo needs a real time span"
        )

    # Cut on row-count quantiles so the partitions carry comparable sample
    # counts, then snap outward to a timestamp edge.
    n = len(ts)
    train_end = ts[min(int(n * train_frac), n - 1)]
    valid_end = ts[min(int(n * (train_frac + valid_frac)), n - 1)]

    train = np.flatnonzero(ts <= train_end)
    valid = np.flatnonzero((ts > train_end + embargo) & (ts <= valid_end))
    test = np.flatnonzero(ts > valid_end + embargo)

    for name, idx in (("train", train), ("valid", valid), ("test", test)):
        if idx.size == 0:
            raise ValueError(
                f"the {name} partition is empty after applying a "
                f"{embargo / np.timedelta64(1, 'm'):.0f} minute embargo; the data "
                "does not span enough time for this horizon"
            )

    embargoed = n - (train.size + valid.size + test.size)
    return Split(
        train=train,
        valid=valid,
        test=test,
        embargo_minutes=int(embargo / np.timedelta64(1, "m")),
        embargoed=embargoed,
    )


def walk_forward_folds(
    ts: np.ndarray,
    horizon: str,
    n_folds: int = 5,
    anchored: bool = True,
    embargo_multiple: float = 2.0,
    min_train_frac: float = 0.3,
) -> list[Split]:
    """Build ``n_folds`` expanding (anchored) or rolling train/test folds.

    Each fold reserves the tail of its training window as a validation slice for
    the temperature fit, because calibrating on the test slice would make the
    reported calibration error meaningless.
    """
    ts = pd.to_datetime(pd.Series(ts), utc=True).to_numpy()
    n = len(ts)
    embargo = np.timedelta64(
        int(round(schema.horizon_minutes(horizon) * embargo_multiple)), "m"
    )
    if n_folds < 2:
        raise ValueError("walk-forward needs at least two folds to be a walk")

    # Test windows tile the tail of the sample; the first fold trains on
    # everything before them.
    start = int(n * min_train_frac)
    edges = np.linspace(start, n, n_folds + 1).astype(int)

    folds: list[Split] = []
    for k in range(n_folds):
        fit_end_row = edges[k]
        test_lo, test_hi = edges[k], edges[k + 1]
        if fit_end_row < 2 or test_hi - test_lo < 1:
            continue
        fit_end = ts[fit_end_row - 1]
        test_end = ts[test_hi - 1]

        fit_rows = np.flatnonzero(ts <= fit_end)
        if not anchored:
            window = edges[k] - edges[0] if k > 0 else fit_end_row
            fit_rows = fit_rows[-max(window, start) :]

        # Hold out the last 20% of the fitting window (by time) for calibration.
        cut = ts[fit_rows[int(len(fit_rows) * 0.8)]]
        train = fit_rows[ts[fit_rows] <= cut]
        valid = fit_rows[ts[fit_rows] > cut + embargo]
        test = np.flatnonzero((ts > fit_end + embargo) & (ts <= test_end))
        if train.size == 0 or valid.size == 0 or test.size == 0:
            continue
        folds.append(
            Split(
                train=train,
                valid=valid,
                test=test,
                embargo_minutes=int(embargo / np.timedelta64(1, "m")),
                embargoed=fit_rows.size - train.size - valid.size,
            )
        )
    if not folds:
        raise ValueError(
            "no usable walk-forward folds; the sample is too short for this "
            "horizon and embargo"
        )
    return folds
