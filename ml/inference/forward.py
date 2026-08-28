"""A Python re-implementation of the artifact forward pass.

This exists to *check* the Go implementation, not to replace it. Nothing in
QuantOS serves predictions from Python; ``internal/mlinfer`` is the only
inference path that reaches a user. What this module buys is the ability to
assert, in a test, that the trainer's idea of the model and the runtime's idea
of the model are the same function — which is otherwise an assumption.

Two forward passes are provided on purpose:

``predict_vector`` reproduces Go's operations in Go's order, one scalar at a
time. It is slow and it is the one the parity test uses, because a vectorised
dot product reassociates the sum and drifts by a few ULP from the Go loop.

``predict_matrix`` is the vectorised path used for bulk evaluation, where a
1e-15 difference in the tenth decimal of a Brier score is not interesting.
``test_go_parity.py`` pins the two together so the fast path cannot silently
diverge.

Mirrors, line for line:
  - mlinfer.Model.PredictVector  (standardise, clamp, score, softmax)
  - mlinfer.softmax              (max-subtract, exp, normalise)
  - mlinfer.Calibration.apply    (temperature / vector scaling)
  - domain.NewDistribution       (non-negative, renormalise, uniform on zero)
  - domain.Distribution.Sharpen  (p ** (1/t), renormalised)
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from ml.features import schema


@dataclass(frozen=True)
class Artifact:
    """The subset of the JSON artifact that inference actually reads."""

    raw: dict

    @staticmethod
    def load(path: str | Path) -> "Artifact":
        return Artifact(json.loads(Path(path).read_text(encoding="utf-8")))

    @property
    def family(self) -> str:
        return self.raw["family"]

    @property
    def features(self) -> list[str]:
        return list(self.raw["features"])

    @property
    def mean(self) -> list[float]:
        return [float(v) for v in self.raw["mean"]]

    @property
    def std(self) -> list[float]:
        return [float(v) for v in self.raw["std"]]

    @property
    def calibration(self) -> dict:
        return self.raw.get("calibration") or {"method": "none"}


def _clamp(v: float, lo: float, hi: float) -> float:
    """domain.Clamp."""
    if v < lo:
        return lo
    if v > hi:
        return hi
    return v


def _new_distribution(up: float, flat: float, down: float) -> list[float]:
    """domain.NewDistribution.

    Note the sum is recomputed as ``up + flat + down`` in that order rather than
    reusing an accumulator, and that invalid input collapses to the uniform
    distribution — the honest answer when the model has nothing to say.
    """

    def non_neg(v: float) -> float:
        if math.isnan(v) or math.isinf(v) or v < 0:
            return 0.0
        return v

    up, flat, down = non_neg(up), non_neg(flat), non_neg(down)
    total = up + flat + down
    if total <= 0 or math.isnan(total) or math.isinf(total):
        return [1.0 / 3, 1.0 / 3, 1.0 / 3]
    return [up / total, flat / total, down / total]


def _softmax(scores: list[float]) -> list[float]:
    """mlinfer.softmax."""
    top = scores[0]
    for s in scores[1:]:
        if s > top:
            top = s
    exp = [math.exp(s - top) for s in scores]
    total = 0.0
    for e in exp:
        total += e
    if total == 0:
        return _new_distribution(1, 1, 1)
    return _new_distribution(exp[0], exp[1], exp[2])


def _sharpen(dist: list[float], t: float) -> list[float]:
    """domain.Distribution.Sharpen. t < 1 sharpens, t > 1 flattens."""
    if t <= 0:
        return list(dist)
    return _new_distribution(*(math.pow(p, 1 / t) for p in dist))


def apply_calibration(dist: list[float], cal: dict) -> list[float]:
    """mlinfer.Calibration.apply.

    Temperature is applied in probability space, exactly as Go does. That is
    algebraically identical to dividing the logits by T, but only the Go
    formulation is bit-for-bit reproducible against the runtime, so this is the
    formulation the parity test compares.
    """
    method = cal.get("method", "none")
    if method == "temperature":
        t = float(cal.get("temperature", 0.0) or 0.0)
        if t > 0 and t != 1:
            return _sharpen(dist, t)
        return list(dist)
    if method == "vector":
        scale = [float(v) for v in cal["scale"]]
        shift = [float(v) for v in cal["shift"]]
        out = []
        for i, p in enumerate(dist):
            lp = math.log(max(p, 1e-12))
            out.append(math.exp(scale[i] * lp + shift[i]))
        return _new_distribution(*out)
    return list(dist)


def standardise(art: Artifact, vec: list[float]) -> list[float]:
    """Standardise and clamp, mirroring the head of PredictVector."""
    mean, std = art.mean, art.std
    return [
        _clamp((v - mean[i]) / std[i], -schema.STANDARDISED_CLAMP, schema.STANDARDISED_CLAMP)
        for i, v in enumerate(vec)
    ]


def raw_scores(art: Artifact, z: list[float]) -> list[float]:
    """Per-class scores before the softmax, in domain.AllOutcomes order."""
    if art.family == schema.FAMILY_LOGISTIC:
        coef = art.raw["coefficients"]
        inter = art.raw["intercepts"]
        scores = []
        for c in range(len(schema.ALL_OUTCOMES)):
            s = float(inter[c])
            row = coef[c]
            # Ascending index, one term at a time: the same association Go uses.
            for i, zi in enumerate(z):
                s += float(row[i]) * zi
            scores.append(s)
        return scores
    if art.family == schema.FAMILY_STUMPS:
        scores = [float(v) for v in art.raw["base_score"]]
        for c, group in enumerate(art.raw["stumps"]):
            for stump in group:
                v = float(stump["l"])
                if z[int(stump["f"])] > float(stump["t"]):
                    v = float(stump["r"])
                scores[c] += v
        return scores
    raise ValueError(f"unsupported family {art.family!r}")


def predict_vector(art: Artifact, vec: list[float] | np.ndarray) -> list[float]:
    """Full forward pass for one feature vector, in mlinfer's operation order.

    Returns ``[P(UP), P(FLAT), P(DOWN)]``.
    """
    vec = [float(v) for v in vec]
    if len(vec) != len(art.features):
        raise ValueError(
            f"expected {len(art.features)} features, got {len(vec)}"
        )
    z = standardise(art, vec)
    dist = _softmax(raw_scores(art, z))
    return apply_calibration(dist, art.calibration)


def predict_matrix(art: Artifact, X: np.ndarray) -> np.ndarray:
    """Vectorised forward pass over an (n, d) matrix. Returns (n, 3)."""
    X = np.asarray(X, dtype=np.float64)
    mean = np.asarray(art.mean, dtype=np.float64)
    std = np.asarray(art.std, dtype=np.float64)
    z = np.clip(
        (X - mean) / std, -schema.STANDARDISED_CLAMP, schema.STANDARDISED_CLAMP
    )

    if art.family == schema.FAMILY_LOGISTIC:
        coef = np.asarray(art.raw["coefficients"], dtype=np.float64)
        inter = np.asarray(art.raw["intercepts"], dtype=np.float64)
        scores = z @ coef.T + inter
    elif art.family == schema.FAMILY_STUMPS:
        scores = np.tile(
            np.asarray(art.raw["base_score"], dtype=np.float64), (z.shape[0], 1)
        )
        for c, group in enumerate(art.raw["stumps"]):
            for stump in group:
                col = z[:, int(stump["f"])]
                scores[:, c] += np.where(
                    col > float(stump["t"]), float(stump["r"]), float(stump["l"])
                )
    else:
        raise ValueError(f"unsupported family {art.family!r}")

    return calibrate_matrix(softmax_matrix(scores), art.calibration)


def softmax_matrix(scores: np.ndarray) -> np.ndarray:
    e = np.exp(scores - scores.max(axis=1, keepdims=True))
    return e / e.sum(axis=1, keepdims=True)


def calibrate_matrix(P: np.ndarray, cal: dict) -> np.ndarray:
    method = cal.get("method", "none")
    if method == "temperature":
        t = float(cal.get("temperature", 0.0) or 0.0)
        if t > 0 and t != 1:
            Q = np.power(P, 1.0 / t)
            return Q / Q.sum(axis=1, keepdims=True)
        return P
    if method == "vector":
        scale = np.asarray(cal["scale"], dtype=np.float64)
        shift = np.asarray(cal["shift"], dtype=np.float64)
        Q = np.exp(scale * np.log(np.maximum(P, 1e-12)) + shift)
        return Q / Q.sum(axis=1, keepdims=True)
    return P
