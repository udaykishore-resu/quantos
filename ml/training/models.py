"""Fitters for the two families ``internal/mlinfer`` can serve, plus calibration.

Both fitters work on *standardised, clamped* inputs, not raw features. That is
not a convenience: ``mlinfer.Model.PredictVector`` clamps z to +/-6 before it
touches a coefficient, so fitting on unclamped z would fit a slightly different
function from the one that gets served. The clamp is part of the model.

Neither fitter draws a random number. Determinism here is not a nice property
to have — the artifact digest is the identity of a deployed model, and an
identity that changes when nothing changed is worthless.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ml.features import schema

N_CLASSES = len(schema.ALL_OUTCOMES)


@dataclass
class Standardiser:
    """The mean/std that go into the artifact, fitted on the training rows only."""

    mean: np.ndarray
    std: np.ndarray

    @staticmethod
    def fit(X: np.ndarray, min_std: float = 1e-8) -> "Standardiser":
        mean = X.mean(axis=0)
        std = X.std(axis=0)
        # A constant feature would make Validate() reject the artifact for a
        # non-positive std. Flooring keeps the column inert rather than failing
        # the run, and the trainer reports which columns were floored.
        std = np.maximum(std, min_std)
        return Standardiser(mean=mean.astype(np.float64), std=std.astype(np.float64))

    def transform(self, X: np.ndarray) -> np.ndarray:
        z = (X - self.mean) / self.std
        return np.clip(z, -schema.STANDARDISED_CLAMP, schema.STANDARDISED_CLAMP)

    def degenerate(self, X: np.ndarray, min_std: float = 1e-8) -> list[int]:
        return [i for i, s in enumerate(X.std(axis=0)) if s <= min_std]


# --- multinomial logistic ---------------------------------------------------


@dataclass
class LogisticFit:
    coefficients: np.ndarray  # (3, d), rows in AllOutcomes order
    intercepts: np.ndarray  # (3,)
    hyperparams: dict[str, float]

    def scores(self, Z: np.ndarray) -> np.ndarray:
        return Z @ self.coefficients.T + self.intercepts


def fit_logistic(
    Z: np.ndarray,
    y: np.ndarray,
    l2: float = 1.0,
    max_iter: int = 500,
    tol: float = 1e-6,
    seed: int = 20260827,
) -> LogisticFit:
    """Fit a 3-class softmax regression on standardised inputs.

    scikit-learn's lbfgs when it is available, a plain full-batch gradient
    descent otherwise. The fallback is not a token: the container this runs in
    has a restricted network, and a trainer that cannot run is not a trainer.
    Both paths minimise the same penalised objective and both are deterministic;
    ``train.py`` records which one was used in the artifact metadata.
    """
    _assert_all_classes_present(y)
    try:
        from sklearn.linear_model import LogisticRegression
    except ImportError:
        return _fit_logistic_numpy(Z, y, l2=l2, max_iter=max_iter, tol=tol)

    # Both paths minimise `sum_i loss_i + (l2/2)*||W||^2`. sklearn parameterises
    # that objective by C = 1/l2; the gradient path below divides through by n
    # and so carries l2/n. Getting this wrong is silent — an over-regularised
    # fit still converges, still validates, and just returns the class prior for
    # every input — so the two conventions are stated here rather than implied.
    clf = LogisticRegression(
        C=1.0 / max(l2, 1e-12),
        solver="lbfgs",
        max_iter=max_iter,
        tol=tol,
        fit_intercept=True,
        random_state=seed,
    )
    clf.fit(Z, y)
    coef, inter = _canonicalise_rows(clf.coef_, clf.intercept_, clf.classes_)
    return LogisticFit(
        coefficients=coef,
        intercepts=inter,
        hyperparams={"l2": float(l2), "max_iter": float(max_iter), "solver_lbfgs": 1.0},
    )


def _canonicalise_rows(
    coef: np.ndarray, intercept: np.ndarray, classes: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    """Reorder fitted rows into domain.AllOutcomes order.

    scikit-learn orders rows by sorted class label. The labels are already the
    AllOutcomes indices so the orders coincide, but asserting it beats assuming
    it: a silently transposed UP and DOWN row is a sign-flipped model that still
    validates, still serves, and is exactly wrong.
    """
    coef = np.atleast_2d(np.asarray(coef, dtype=np.float64))
    intercept = np.atleast_1d(np.asarray(intercept, dtype=np.float64))
    if coef.shape[0] != N_CLASSES:
        raise ValueError(
            f"expected {N_CLASSES} coefficient rows, got {coef.shape[0]}; a binary "
            "fit means the training sample was missing a class"
        )
    order = np.argsort(np.asarray(classes))
    if not np.array_equal(np.asarray(classes)[order], np.arange(N_CLASSES)):
        raise ValueError(f"unexpected class labels from the fitter: {classes}")
    return coef[order].copy(), intercept[order].copy()


def _fit_logistic_numpy(
    Z: np.ndarray, y: np.ndarray, l2: float, max_iter: int, tol: float
) -> LogisticFit:
    """Full-batch gradient descent with Nesterov momentum, no RNG anywhere."""
    n, d = Z.shape
    W = np.zeros((N_CLASSES, d), dtype=np.float64)
    b = np.zeros(N_CLASSES, dtype=np.float64)
    vW = np.zeros_like(W)
    vb = np.zeros_like(b)
    onehot = np.zeros((n, N_CLASSES), dtype=np.float64)
    onehot[np.arange(n), y] = 1.0

    # Inputs are clamped to +/-6, so the Lipschitz constant of the softmax loss
    # is bounded and a fixed step is safe without a line search.
    lr = 0.5 / (1.0 + (Z**2).sum(axis=1).mean())
    momentum = 0.9
    prev = np.inf
    for _ in range(max_iter):
        Wl, bl = W + momentum * vW, b + momentum * vb
        S = Z @ Wl.T + bl
        S -= S.max(axis=1, keepdims=True)
        E = np.exp(S)
        P = E / E.sum(axis=1, keepdims=True)
        R = (P - onehot) / n
        gW = R.T @ Z + (l2 / n) * Wl
        gb = R.sum(axis=0)
        vW = momentum * vW - lr * gW
        vb = momentum * vb - lr * gb
        W += vW
        b += vb
        loss = -np.log(np.clip(P[np.arange(n), y], 1e-12, 1.0)).mean() + (
            0.5 * l2 / n
        ) * float((W**2).sum())
        if abs(prev - loss) < tol:
            break
        prev = loss
    return LogisticFit(
        coefficients=W,
        intercepts=b,
        hyperparams={"l2": float(l2), "max_iter": float(max_iter), "solver_lbfgs": 0.0},
    )


# --- gradient-boosted stumps ------------------------------------------------


@dataclass
class StumpFit:
    stumps: list[list[dict]]  # per class, in AllOutcomes order
    base_score: np.ndarray
    hyperparams: dict[str, float]

    def scores(self, Z: np.ndarray) -> np.ndarray:
        S = np.tile(self.base_score, (Z.shape[0], 1))
        for c, group in enumerate(self.stumps):
            for st in group:
                col = Z[:, st["f"]]
                S[:, c] += np.where(col > st["t"], st["r"], st["l"])
        return S


def fit_stumps(
    Z: np.ndarray,
    y: np.ndarray,
    rounds: int = 60,
    learning_rate: float = 0.1,
    n_thresholds: int = 32,
    seed: int = 20260827,
) -> StumpFit:
    """Multiclass gradient boosting with depth-one trees.

    One stump per class per round, fitted by least squares to the softmax
    residual, which is the standard multiclass construction. Candidate splits
    are fixed quantiles of the *training* z-distribution, computed once: that
    makes the search exhaustive over a fixed grid rather than dependent on a
    subsample, so the fit is deterministic without needing a seed at all. The
    seed is accepted so every entry point in this tree has the same signature.
    """
    _assert_all_classes_present(y)
    del seed  # no stochastic step to seed; kept for a uniform interface

    n, d = Z.shape
    onehot = np.zeros((n, N_CLASSES), dtype=np.float64)
    onehot[np.arange(n), y] = 1.0

    # Prior class log-odds as the starting score, so the first round corrects a
    # distribution rather than inventing one.
    prior = np.clip(onehot.mean(axis=0), 1e-6, 1.0)
    base = np.log(prior)
    base -= base.mean()

    thresholds, bin_of = _quantile_bins(Z, n_thresholds)
    counts = np.stack([np.bincount(bin_of[:, j], minlength=n_thresholds) for j in range(d)])

    F = np.tile(base, (n, 1))
    stumps: list[list[dict]] = [[] for _ in range(N_CLASSES)]
    # Friedman's multiclass shrinkage: a least-squares fit to the residual
    # overshoots by roughly K/(K-1) under the softmax loss.
    shrink = learning_rate * (N_CLASSES - 1) / N_CLASSES

    for _ in range(rounds):
        S = F - F.max(axis=1, keepdims=True)
        E = np.exp(S)
        P = E / E.sum(axis=1, keepdims=True)
        residual = onehot - P
        for c in range(N_CLASSES):
            st = _best_stump(residual[:, c], bin_of, counts, thresholds, n_thresholds)
            if st is None:
                continue
            st = {
                "f": st["f"],
                "t": st["t"],
                "l": st["l"] * shrink,
                "r": st["r"] * shrink,
            }
            stumps[c].append(st)
            col = Z[:, st["f"]]
            F[:, c] += np.where(col > st["t"], st["r"], st["l"])

    return StumpFit(
        stumps=stumps,
        base_score=base,
        hyperparams={
            "rounds": float(rounds),
            "learning_rate": float(learning_rate),
            "n_thresholds": float(n_thresholds),
        },
    )


def _quantile_bins(Z: np.ndarray, n_bins: int) -> tuple[np.ndarray, np.ndarray]:
    """Bucket each column by quantile, returning (edges, bin index per row)."""
    d = Z.shape[1]
    edges = np.zeros((d, n_bins), dtype=np.float64)
    bin_of = np.zeros(Z.shape, dtype=np.int64)
    qs = np.linspace(0.0, 1.0, n_bins + 1)[1:-1]
    for j in range(d):
        cuts = np.unique(np.quantile(Z[:, j], qs))
        edges[j, : len(cuts)] = cuts
        edges[j, len(cuts) :] = cuts[-1] if len(cuts) else 0.0
        bin_of[:, j] = np.digitize(Z[:, j], cuts, right=False)
    return edges, bin_of


def _best_stump(
    residual: np.ndarray,
    bin_of: np.ndarray,
    counts: np.ndarray,
    thresholds: np.ndarray,
    n_bins: int,
) -> dict | None:
    """Exhaustive least-squares split search over the precomputed bin grid.

    Evaluating every threshold from bin sums is what makes this affordable:
    the cost is one bincount per feature per round rather than a sort.
    """
    d = bin_of.shape[1]
    total = residual.sum()
    n = residual.shape[0]
    best = None
    best_gain = 0.0
    for j in range(d):
        sums = np.bincount(bin_of[:, j], weights=residual, minlength=n_bins)
        cs = np.cumsum(sums)[:-1]
        cn = np.cumsum(counts[j])[:-1]
        valid = (cn > 0) & (cn < n)
        if not valid.any():
            continue
        left_s, left_n = cs[valid], cn[valid]
        right_s, right_n = total - left_s, n - left_n
        gain = left_s**2 / left_n + right_s**2 / right_n - total**2 / n
        k = int(np.argmax(gain))
        if gain[k] > best_gain:
            idx = int(np.flatnonzero(valid)[k])
            best_gain = float(gain[k])
            best = {
                "f": j,
                # Splitting strictly above the bin's upper edge mirrors Go's
                # `if z[f] > t` comparison exactly.
                "t": float(thresholds[j, idx]),
                "l": float(left_s[k] / left_n[k]),
                "r": float(right_s[k] / right_n[k]),
            }
    return best


def _assert_all_classes_present(y: np.ndarray) -> None:
    counts = np.bincount(y, minlength=N_CLASSES)
    missing = [schema.ALL_OUTCOMES[i] for i in range(N_CLASSES) if counts[i] == 0]
    if missing:
        raise ValueError(
            f"the training sample has no {missing} rows. A model that cannot "
            "express a class must not be shipped as a 3-class model."
        )


# --- temperature calibration ------------------------------------------------


def apply_temperature(P: np.ndarray, t: float) -> np.ndarray:
    """The Go transform: p ** (1/t), renormalised. t > 1 flattens."""
    if t <= 0 or t == 1:
        return P
    Q = np.power(np.clip(P, 1e-300, 1.0), 1.0 / t)
    return Q / Q.sum(axis=1, keepdims=True)


def fit_temperature(
    P: np.ndarray,
    y: np.ndarray,
    lo: float = 0.2,
    hi: float = 20.0,
    iterations: int = 80,
) -> float:
    """Choose the temperature that minimises validation negative log likelihood.

    Deterministic golden-section search on log T, over a unimodal objective.
    Fitted on the validation split and never on test, because a calibration
    error measured on the data the calibrator saw is not an error measurement.

    Values above 1 flatten the distribution. On short-horizon price direction
    that is the usual and correct outcome: the raw softmax is overconfident, and
    the risk engine gates on ``Confidence()``, so an uncorrected temperature
    converts noise into signals.
    """
    if len(y) == 0:
        return 1.0

    def nll(log_t: float) -> float:
        Q = apply_temperature(P, float(np.exp(log_t)))
        return float(-np.log(np.clip(Q[np.arange(len(y)), y], 1e-12, 1.0)).mean())

    a, b = float(np.log(lo)), float(np.log(hi))
    inv_phi = (np.sqrt(5.0) - 1.0) / 2.0
    c, d = b - inv_phi * (b - a), a + inv_phi * (b - a)
    fc, fd = nll(c), nll(d)
    for _ in range(iterations):
        if fc < fd:
            b, d, fd = d, c, fc
            c = b - inv_phi * (b - a)
            fc = nll(c)
        else:
            a, c, fc = c, d, fd
            d = a + inv_phi * (b - a)
            fd = nll(d)
        if b - a < 1e-9:
            break
    return float(np.exp((a + b) / 2.0))
