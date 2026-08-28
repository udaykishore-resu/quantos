"""Calibration behaviour, including the monotonicity the risk engine relies on.

``risk.checkConfidence`` thresholds on the model's stated confidence and
``signal.Engine`` thresholds on the directional edge. Both assume that
calibration reweights a distribution without reordering it: a transform that
could turn the second-most-likely class into the argmax would silently change
the *direction* of a signal while claiming only to fix its confidence.
"""

from __future__ import annotations

import math

import numpy as np
import pytest

from ml.evaluation import metrics
from ml.inference import forward
from ml.training import models


def _dists(n: int = 200) -> np.ndarray:
    """A deterministic spread of distributions, from near-uniform to near-certain."""
    rng = np.random.default_rng(20260827)
    logits = rng.normal(0, 2.5, size=(n, 3))
    return forward.softmax_matrix(logits)


@pytest.mark.parametrize("t", [0.25, 0.5, 0.9, 1.0, 1.5, 4.0, 20.0])
def test_temperature_preserves_class_order(t):
    P = _dists()
    Q = models.apply_temperature(P, t)
    assert np.array_equal(np.argsort(P, axis=1), np.argsort(Q, axis=1))
    assert np.allclose(Q.sum(axis=1), 1.0)
    assert (Q >= 0).all()


def test_temperature_above_one_flattens_and_below_one_sharpens():
    P = _dists()
    ent = lambda D: -(D * np.log(np.clip(D, 1e-12, 1))).sum(axis=1)  # noqa: E731
    assert (ent(models.apply_temperature(P, 2.0)) >= ent(P) - 1e-12).all()
    assert (ent(models.apply_temperature(P, 0.5)) <= ent(P) + 1e-12).all()


def test_temperature_is_monotone_in_confidence():
    """Raising T never raises the top probability of any row."""
    P = _dists()
    prev = models.apply_temperature(P, 0.5).max(axis=1)
    for t in (0.75, 1.0, 1.5, 3.0, 8.0):
        cur = models.apply_temperature(P, t).max(axis=1)
        assert (cur <= prev + 1e-12).all()
        prev = cur


def test_probability_space_temperature_equals_logit_scaling():
    """Go applies p**(1/T) in probability space; that must be the same function
    as dividing the logits by T, or the artifact's temperature would mean one
    thing in research and another in production."""
    rng = np.random.default_rng(7)
    logits = rng.normal(0, 3, size=(500, 3))
    P = forward.softmax_matrix(logits)
    for t in (0.3, 1.7, 6.0):
        assert np.abs(
            models.apply_temperature(P, t) - forward.softmax_matrix(logits / t)
        ).max() < 1e-12


def test_fit_temperature_recovers_a_known_miscalibration():
    """Take a calibrated model, deliberately sharpen it, and check the fit
    undoes it. A calibrator that cannot recover a synthetic distortion is not
    going to fix a real one."""
    rng = np.random.default_rng(20260827)
    logits = rng.normal(0, 1.2, size=(30000, 3))
    P_true = forward.softmax_matrix(logits)
    y = np.array([rng.choice(3, p=row) for row in P_true])

    # Halving the logits' scale doubles their spread; undoing it needs T = 2,
    # because apply_temperature(P, t) is softmax(logits_P / t).
    overconfident = forward.softmax_matrix(logits / 0.5)
    t = models.fit_temperature(overconfident, y)
    assert 1.8 < t < 2.2, f"expected to recover T near 2.0, got {t}"

    before, _ = metrics.expected_calibration_error(overconfident, y)
    after, _ = metrics.expected_calibration_error(
        models.apply_temperature(overconfident, t), y
    )
    assert after < before


def test_fit_temperature_is_deterministic():
    P, y = _dists(5000), np.arange(5000) % 3
    assert models.fit_temperature(P, y) == models.fit_temperature(P, y)


def test_calibration_never_manufactures_a_direction():
    """Whatever the temperature, P(up) - P(down) may shrink or grow but must
    never change sign: the calibrator is not allowed to flip a call."""
    P = _dists(500)
    edge = P[:, 0] - P[:, 2]
    for t in (0.2, 0.8, 1.0, 3.0, 15.0):
        e = models.apply_temperature(P, t)
        assert (np.sign(e[:, 0] - e[:, 2]) == np.sign(edge)).all()


def test_ece_is_zero_for_a_perfectly_calibrated_predictor():
    """A predictor whose confidence equals its accuracy by construction should
    score near zero, or the metric is measuring something else."""
    rng = np.random.default_rng(11)
    P = forward.softmax_matrix(rng.normal(0, 1.5, size=(200000, 3)))
    y = np.array([rng.choice(3, p=row) for row in P])
    ece, max_dev = metrics.expected_calibration_error(P, y)
    assert ece < 0.01 and max_dev < 0.05


def test_ece_detects_overconfidence():
    rng = np.random.default_rng(11)
    P = forward.softmax_matrix(rng.normal(0, 1.5, size=(50000, 3)))
    y = np.array([rng.choice(3, p=row) for row in P])
    ece_honest, _ = metrics.expected_calibration_error(P, y)
    ece_liar, _ = metrics.expected_calibration_error(
        models.apply_temperature(P, 0.25), y
    )
    assert ece_liar > ece_honest + 0.05


def test_go_temperature_semantics_are_a_no_op_at_exactly_one():
    """mlinfer.Calibration.apply skips the transform when T == 1, so the Python
    forward pass must skip it too or the two disagree on the identity case."""
    cal = {"method": "temperature", "temperature": 1.0}
    d = [0.5, 0.3, 0.2]
    assert forward.apply_calibration(d, cal) == d


def test_vector_calibration_matches_the_go_formulation():
    cal = {"method": "vector", "scale": [1.1, 0.9, 1.0], "shift": [0.05, -0.02, 0.0]}
    d = [0.5, 0.3, 0.2]
    got = forward.apply_calibration(d, cal)
    want = [
        math.exp(cal["scale"][i] * math.log(max(d[i], 1e-12)) + cal["shift"][i])
        for i in range(3)
    ]
    total = sum(want)
    assert np.allclose(got, [w / total for w in want])
    assert abs(sum(got) - 1.0) < 1e-12
