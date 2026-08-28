"""Scoring rules and calibration diagnostics, mirroring the Go definitions.

The Go side computes the same quantities in ``domain.BrierScore`` and
``domain.LogLoss`` and the evaluation service reports them; keeping the Python
definitions identical means a research number and a production number are
comparable rather than merely similar.

Accuracy never appears here without a base rate. A three-class problem whose
majority class holds 40% of the mass makes 40% accuracy look like signal, and
reporting it alone is the most common way a research result lies.
"""

from __future__ import annotations

import numpy as np

from ml.features import schema

N_CLASSES = len(schema.ALL_OUTCOMES)


def accuracy(P: np.ndarray, y: np.ndarray) -> float:
    return float((np.argmax(P, axis=1) == y).mean())


def base_rate(y: np.ndarray) -> float:
    """Accuracy of always predicting the most common class in this sample.

    This is the bar. A model that does not clear it has learned nothing worth
    serving, however good its log loss looks.
    """
    counts = np.bincount(y, minlength=N_CLASSES)
    return float(counts.max() / max(counts.sum(), 1))


def majority_class(y: np.ndarray) -> str:
    counts = np.bincount(y, minlength=N_CLASSES)
    return schema.ALL_OUTCOMES[int(np.argmax(counts))]


def brier(P: np.ndarray, y: np.ndarray) -> float:
    """Multiclass Brier score, matching domain.BrierScore. 0 best, 2 worst."""
    onehot = np.zeros_like(P)
    onehot[np.arange(len(y)), y] = 1.0
    return float(((P - onehot) ** 2).sum(axis=1).mean())


def log_loss(P: np.ndarray, y: np.ndarray) -> float:
    """Mean negative log likelihood, clipped exactly as domain.LogLoss clips."""
    p = np.clip(P[np.arange(len(y)), y], 1e-12, 1.0)
    return float(-np.log(p).mean())


def uniform_log_loss() -> float:
    """Log loss of the maximum-entropy answer: the weakest baseline there is."""
    return float(np.log(N_CLASSES))


def class_prior_log_loss(y: np.ndarray) -> float:
    """Log loss of the best constant predictor on this sample.

    Beating the uniform distribution is easy — it only requires knowing that
    FLAT is rarer than the directional classes. Beating the sample's own class
    prior requires the features to carry information about *this* row, which is
    the claim a model is actually making.
    """
    counts = np.bincount(y, minlength=N_CLASSES).astype(np.float64)
    prior = np.clip(counts / max(counts.sum(), 1), 1e-12, 1.0)
    return float(-np.log(prior[y]).mean())


def expected_calibration_error(
    P: np.ndarray, y: np.ndarray, bins: int = 15
) -> tuple[float, float]:
    """Return (ECE, max bin deviation) over the top-class confidence.

    Equal-width bins on [0,1]. Confidence is the probability assigned to the
    predicted class, and the bin's empirical accuracy is what that probability
    claims. The gap between them is the part a risk threshold cares about,
    because ``risk.MinConfidence`` gates on this number believing itself.
    """
    conf = P.max(axis=1)
    correct = (np.argmax(P, axis=1) == y).astype(np.float64)
    edges = np.linspace(0.0, 1.0, bins + 1)
    idx = np.clip(np.digitize(conf, edges[1:-1], right=False), 0, bins - 1)

    ece = 0.0
    max_dev = 0.0
    n = len(conf)
    for b in range(bins):
        mask = idx == b
        m = int(mask.sum())
        if m == 0:
            continue
        gap = abs(float(correct[mask].mean()) - float(conf[mask].mean()))
        ece += (m / n) * gap
        max_dev = max(max_dev, gap)
    return float(ece), float(max_dev)


def reliability_table(P: np.ndarray, y: np.ndarray, bins: int = 10) -> list[dict]:
    """Per-bin counts, mean confidence and empirical accuracy, for the report."""
    conf = P.max(axis=1)
    correct = (np.argmax(P, axis=1) == y).astype(np.float64)
    edges = np.linspace(0.0, 1.0, bins + 1)
    idx = np.clip(np.digitize(conf, edges[1:-1], right=False), 0, bins - 1)
    out = []
    for b in range(bins):
        mask = idx == b
        m = int(mask.sum())
        if m == 0:
            continue
        out.append(
            {
                "bin_low": round(float(edges[b]), 4),
                "bin_high": round(float(edges[b + 1]), 4),
                "count": m,
                "mean_confidence": round(float(conf[mask].mean()), 6),
                "empirical_accuracy": round(float(correct[mask].mean()), 6),
            }
        )
    return out


def confusion(P: np.ndarray, y: np.ndarray) -> list[list[int]]:
    """Rows are actual classes, columns predicted, both in AllOutcomes order."""
    pred = np.argmax(P, axis=1)
    m = np.zeros((N_CLASSES, N_CLASSES), dtype=np.int64)
    for a, p in zip(y, pred):
        m[a, p] += 1
    return m.tolist()


def per_regime_accuracy(
    P: np.ndarray, y: np.ndarray, regimes: np.ndarray
) -> dict[str, dict]:
    """Accuracy against base rate, split by market regime.

    A model can clear the overall base rate purely by being right in the one
    regime that dominates the sample; the per-regime table is where that shows.
    """
    out: dict[str, dict] = {}
    for r in sorted(set(map(str, regimes))):
        mask = np.asarray([str(x) == r for x in regimes])
        if not mask.any():
            continue
        acc = accuracy(P[mask], y[mask])
        base = base_rate(y[mask])
        out[r] = {
            "n": int(mask.sum()),
            "accuracy": round(acc, 6),
            "base_rate": round(base, 6),
            "lift": round(acc - base, 6),
        }
    return out


def class_distribution(y: np.ndarray) -> dict[str, float]:
    counts = np.bincount(y, minlength=N_CLASSES)
    total = max(int(counts.sum()), 1)
    return {
        schema.ALL_OUTCOMES[i]: round(float(counts[i] / total), 6)
        for i in range(N_CLASSES)
    }


def summarise(P: np.ndarray, y: np.ndarray, regimes: np.ndarray | None = None) -> dict:
    """The block of numbers every report in this tree quotes."""
    acc = accuracy(P, y)
    base = base_rate(y)
    ece, max_dev = expected_calibration_error(P, y)
    out = {
        "n": int(len(y)),
        "accuracy": round(acc, 6),
        "base_rate": round(base, 6),
        "majority_class": majority_class(y),
        # Lift is stated as a difference, not a ratio: "1.02x the base rate"
        # reads as a result, "+0.8 points" reads as what it is.
        "lift_over_base_rate": round(acc - base, 6),
        "beats_base_rate": bool(acc > base),
        "brier": round(brier(P, y), 6),
        "log_loss": round(log_loss(P, y), 6),
        "log_loss_uniform": round(uniform_log_loss(), 6),
        "log_loss_class_prior": round(class_prior_log_loss(y), 6),
        "beats_class_prior_log_loss": bool(log_loss(P, y) < class_prior_log_loss(y)),
        "ece": round(ece, 6),
        "max_calibration_deviation": round(max_dev, 6),
        "class_distribution": class_distribution(y),
        "confusion_actual_by_predicted": confusion(P, y),
    }
    if regimes is not None:
        out["per_regime"] = per_regime_accuracy(P, y, regimes)
    return out
