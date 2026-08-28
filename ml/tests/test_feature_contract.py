"""The training/serving skew guard.

If any test in this file fails, a model fitted by this tree would be indexed
differently from the way ``internal/mlinfer`` indexes it at serve time. That
does not degrade accuracy gracefully; it produces confident output from
transposed inputs.
"""

from __future__ import annotations

import pytest

from ml.features import dataset, schema


def test_python_mirror_matches_go_source(repo_root):
    """The hard-coded list must equal the one the Go compiler uses."""
    go = schema.go_model_feature_set(
        repo_root / "internal" / "domain" / "features.go"
    )
    assert go == schema.MODEL_FEATURE_SET
    assert len(go) == 16, "the shipped model consumes exactly 16 features"


def test_outcome_order_matches_go_source(repo_root):
    """Row 0 is UP, row 1 FLAT, row 2 DOWN — softmax() unpacks them positionally."""
    go = schema.go_all_outcomes(repo_root / "internal" / "domain" / "prediction.go")
    assert go == schema.ALL_OUTCOMES == ["UP", "FLAT", "DOWN"]


def test_verify_against_go_source_passes(repo_root):
    note = schema.verify_against_go_source(repo_root)
    assert "verified" in note


def test_loader_accepts_the_exporter_header(synthetic_csv):
    ds = dataset.load(synthetic_csv, horizon="60m")
    assert list(ds.frame.columns[4:20]) == schema.MODEL_FEATURE_SET
    assert ds.flat_band_bps == 15.0


def test_loader_rejects_a_reordered_feature_block(synthetic_csv, tmp_path):
    """Same names, wrong order: a set-membership check would pass this."""
    lines = synthetic_csv.read_text().splitlines()
    header = lines[0].split(",")
    header[4], header[5] = header[5], header[4]
    bad = tmp_path / "reordered.csv"
    bad.write_text(",".join(header) + "\n" + "\n".join(lines[1:]) + "\n")

    with pytest.raises(ValueError) as err:
        dataset.load(bad, horizon="60m")
    assert "does not match domain.ModelFeatureSet" in str(err.value)
    assert "wrong order" in str(err.value)


def test_loader_rejects_a_missing_feature(synthetic_csv, tmp_path):
    lines = synthetic_csv.read_text().splitlines()
    drop = schema.MODEL_FEATURE_SET.index("rsi_14") + 4
    rows = [",".join(c for i, c in enumerate(ln.split(",")) if i != drop) for ln in lines]
    bad = tmp_path / "missing.csv"
    bad.write_text("\n".join(rows) + "\n")

    with pytest.raises(ValueError) as err:
        dataset.load(bad, horizon="60m")
    assert "rsi_14" in str(err.value)


def test_loader_rejects_a_flat_band_that_contradicts_the_labels(synthetic_csv):
    """The band is the class definition; taking it on trust from a flag is how a
    model ends up stamped with a threshold it was never trained against."""
    with pytest.raises(ValueError) as err:
        dataset.load(synthetic_csv, horizon="60m", declared_flat_band_bps=40.0)
    assert "inconsistent with the declared flat band" in str(err.value)


def test_labels_map_to_the_go_outcome_indices(synthetic_csv):
    ds = dataset.load(synthetic_csv, horizon="60m")
    y = ds.labels()
    ret = ds.forward_returns()
    band = ds.flat_band_bps
    assert set(y.tolist()) <= {0, 1, 2}
    assert (ret[y == 0] > band).all(), "class 0 must be UP"
    assert (abs(ret[y == 1]) <= band).all(), "class 1 must be FLAT"
    assert (ret[y == 2] < -band).all(), "class 2 must be DOWN"
