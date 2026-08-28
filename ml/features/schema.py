"""The feature contract shared with the Go feature engine.

This module exists for one reason: to make training/serving skew a loud,
immediate failure instead of a quiet accuracy regression.

The Go side owns the definition (``internal/domain/features.go``,
``domain.ModelFeatureSet``). Python mirrors it here, and
:func:`verify_against_go_source` re-derives the list from the Go source so that
the mirror cannot rot. A model fitted on a feature list that differs from the
one ``mlinfer`` will serve produces confident nonsense, which is strictly worse
than refusing to train.
"""

from __future__ import annotations

import re
from pathlib import Path

# Mirror of domain.ModelFeatureSet. Order is part of the contract: the same
# names in a different order is a different model, because mlinfer indexes the
# coefficient rows positionally.
MODEL_FEATURE_SET: list[str] = [
    "ret_log_1",
    "ret_log_5",
    "momentum_10",
    "momentum_60",
    "rsi_14",
    "macd_hist",
    "vwap_dist_bps",
    "bb_percent_b",
    "adx_14",
    "rel_strength_20",
    "volume_ratio_20",
    "volume_z_20",
    "atr_pct",
    "realized_vol_20",
    "zscore_20",
    "trend_slope_20",
]

# Mirror of domain.AllOutcomes. The index of a class here is the row index of
# its coefficients in the artifact and the argument position in Go's
# NewDistribution(up, flat, down).
ALL_OUTCOMES: list[str] = ["UP", "FLAT", "DOWN"]

# Mirror of domain.Horizon values that cmd/quantos export-features emits.
DEFAULT_HORIZONS: list[str] = ["15m", "60m", "1d"]

# Mirror of the clamp applied to standardised inputs in
# mlinfer.Model.PredictVector. Training applies the identical clamp, so the
# fitted function is the function that will be served.
STANDARDISED_CLAMP = 6.0

# Mirror of domain.ModelFamily.
FAMILY_LOGISTIC = "multinomial_logistic"
FAMILY_STUMPS = "gradient_boosted_stumps"

# Mirror of mlinfer.ArtifactSchemaVersion.
ARTIFACT_SCHEMA_VERSION = 1


def horizon_minutes(horizon: str) -> int:
    """Return a horizon's length in minutes, mirroring domain.Horizon.Duration."""
    table = {"15m": 15, "60m": 60, "1d": 24 * 60, "5d": 5 * 24 * 60}
    if horizon not in table:
        raise ValueError(
            f"unknown horizon {horizon!r}; the Go side understands {sorted(table)}"
        )
    return table[horizon]


def expected_csv_header(horizons: list[str] | None = None) -> list[str]:
    """Return the exact header ``cmd/quantos export-features`` writes.

    Kept as a function rather than a constant because the loader compares the
    whole header, not just the feature block: a renamed ``regime`` column or a
    reordered label pair is the same class of bug.
    """
    horizons = horizons if horizons is not None else DEFAULT_HORIZONS
    header = ["ts", "ticker", "close", "regime"] + list(MODEL_FEATURE_SET)
    for h in horizons:
        header += [f"fwd_ret_bps_{h}", f"label_{h}"]
    return header


# Matches the Feat* constant block, e.g. `FeatRSI14 = "rsi_14"`.
_CONST_RE = re.compile(r'^\s*(Feat\w+)\s*=\s*"([^"]+)"', re.MULTILINE)
# Matches the body of `var ModelFeatureSet = []string{ ... }`.
_SET_RE = re.compile(r"var\s+ModelFeatureSet\s*=\s*\[\]string\{(.*?)\}", re.DOTALL)
_OUTCOME_RE = re.compile(r"var\s+AllOutcomes\s*=\s*\[\]Outcome\{(.*?)\}", re.DOTALL)
_OUTCOME_CONST_RE = re.compile(r'^\s*(Outcome\w+)\s+Outcome\s*=\s*"([^"]+)"', re.MULTILINE)


def go_model_feature_set(features_go: Path) -> list[str]:
    """Re-derive domain.ModelFeatureSet by parsing the Go source.

    Parsing beats importing a duplicated JSON file: there is exactly one
    definition, and it is the one the compiler uses.
    """
    src = features_go.read_text(encoding="utf-8")
    consts = {name: value for name, value in _CONST_RE.findall(src)}
    body = _SET_RE.search(src)
    if body is None:
        raise ValueError(f"{features_go}: could not find `var ModelFeatureSet`")
    names: list[str] = []
    for token in _identifiers(body.group(1)):
        if token not in consts:
            raise ValueError(
                f"{features_go}: ModelFeatureSet references {token!r}, "
                "which is not a Feat* constant this parser recognises"
            )
        names.append(consts[token])
    return names


def _identifiers(body: str) -> list[str]:
    """Split a Go composite-literal body into its element identifiers.

    Handles both one-per-line and single-line literals, and drops comments.
    """
    out: list[str] = []
    for line in body.splitlines():
        for token in line.split("//", 1)[0].split(","):
            token = token.strip()
            if token:
                out.append(token)
    return out


def go_all_outcomes(prediction_go: Path) -> list[str]:
    """Re-derive domain.AllOutcomes by parsing the Go source."""
    src = prediction_go.read_text(encoding="utf-8")
    consts = {name: value for name, value in _OUTCOME_CONST_RE.findall(src)}
    body = _OUTCOME_RE.search(src)
    if body is None:
        raise ValueError(f"{prediction_go}: could not find `var AllOutcomes`")
    out: list[str] = []
    for token in _identifiers(body.group(1)):
        if token not in consts:
            raise ValueError(
                f"{prediction_go}: AllOutcomes references unknown constant {token!r}"
            )
        out.append(consts[token])
    return out


def repo_root(start: Path | None = None) -> Path | None:
    """Walk up from ``start`` looking for the module's go.mod.

    Returns None when the research tree has been copied somewhere without the
    Go source; callers treat that as "cannot verify" rather than "verified".
    """
    here = (start or Path(__file__)).resolve()
    for candidate in [here, *here.parents]:
        if (candidate / "go.mod").is_file():
            return candidate
    return None


def verify_against_go_source(root: Path | None = None) -> str:
    """Assert the Python mirrors match the Go definitions.

    Raises ``RuntimeError`` on any divergence. Returns a human-readable note
    recording what was checked, which the trainer stamps into the artifact so a
    reader can tell whether the guard actually ran.
    """
    root = root or repo_root()
    if root is None:
        return "feature contract unverified: Go source not found next to ml/"

    features_go = root / "internal" / "domain" / "features.go"
    prediction_go = root / "internal" / "domain" / "prediction.go"
    if not features_go.is_file() or not prediction_go.is_file():
        return "feature contract unverified: internal/domain sources missing"

    problems: list[str] = []
    go_features = go_model_feature_set(features_go)
    if go_features != MODEL_FEATURE_SET:
        problems.append(
            "MODEL_FEATURE_SET diverged from domain.ModelFeatureSet\n"
            f"  go:     {go_features}\n"
            f"  python: {MODEL_FEATURE_SET}"
        )
    go_outcomes = go_all_outcomes(prediction_go)
    if go_outcomes != ALL_OUTCOMES:
        problems.append(
            "ALL_OUTCOMES diverged from domain.AllOutcomes\n"
            f"  go:     {go_outcomes}\n"
            f"  python: {ALL_OUTCOMES}"
        )
    if problems:
        raise RuntimeError(
            "training/serving skew guard failed:\n  - " + "\n  - ".join(problems)
        )
    return f"feature contract verified against {features_go.relative_to(root)}"
