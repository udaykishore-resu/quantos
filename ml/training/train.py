#!/usr/bin/env python3
"""Fit the 3-class direction model and write an artifact ``mlinfer`` can load.

Usage (this is exactly what `make train` runs):

    python3 ml/training/train.py --data ml/data/features.csv --out ml/artifacts \
        --model-id direction-3class --horizon 60m

What the artifact promises, and what this script therefore has to guarantee:

  * The feature list is ``domain.ModelFeatureSet``, in order, verified against
    the Go source rather than copied from it.
  * The standardiser in the artifact is the standardiser the fit used, computed
    on training rows only.
  * The calibration temperature was fitted on the validation split, and the
    accuracy and calibration figures reported are from a test split that no
    fitting step ever saw.
  * The artifact carries its own report card. ``metadata.test_samples``,
    ``test_accuracy``, ``test_base_rate`` and ``test_brier`` come from the test
    split and nowhere else; ``mlinfer.Artifact.Validate`` refuses to load an
    artifact that omits them, and ``ProvisionalHealth`` marks the model unhealthy
    when the accuracy does not beat the base rate it is reported against.
  * ``calibration.ece`` and ``calibration.max_deviation`` come from the
    validation split, because they describe the temperature that was fitted
    there. ``ProvisionalHealth`` blocks a model whose ``max_deviation`` exceeds
    the 0.10 promotion gate.
  * Two runs over the same CSV produce byte-identical files apart from
    ``metadata.trained_at``. Every float is rounded to 12 significant digits
    before it is written, so a last-bit difference in a BLAS reduction cannot
    change the artifact's identity.
  * ``metadata.generator`` says this trainer produced it, so a fitted model can
    never be mistaken for the synthetic bootstrap artifact.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from ml.evaluation import metrics  # noqa: E402
from ml.features import dataset, schema  # noqa: E402
from ml.inference import forward  # noqa: E402
from ml.training import models  # noqa: E402

GENERATOR = "quantos/ml/training/train.py"
#: Everything written to the artifact is rounded here first. 12 significant
#: digits is far more precision than a fitted coefficient carries and far less
#: than a float64's last-bit noise, which is the point.
SIGNIFICANT_DIGITS = 12


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    rng_note = f"seed={args.seed}"

    # Guard first, work second: if the Python mirror of the feature contract has
    # drifted from the Go definition, nothing downstream is worth computing.
    contract_note = schema.verify_against_go_source()
    print(f"[contract] {contract_note}")

    ds = dataset.load(
        args.data, horizon=args.horizon, declared_flat_band_bps=args.flat_band_bps
    )
    print(
        f"[data] {ds.n} rows from {ds.source} "
        f"({ds.frame['ticker'].nunique()} symbols, horizon {ds.horizon}, "
        f"FLAT band +/-{ds.flat_band_bps:g} bps, sha256 {ds.data_digest[:12]})"
    )

    X, y, ts = ds.matrix(), ds.labels(), ds.timestamps()
    split = dataset.chronological_split(
        ts,
        horizon=args.horizon,
        train_frac=args.train_frac,
        valid_frac=args.valid_frac,
        embargo_multiple=args.embargo_multiple,
    )
    bounds = split.bounds(ts)
    print(
        f"[split] train={split.train.size} valid={split.valid.size} "
        f"test={split.test.size} embargoed={split.embargoed} "
        f"(gap {split.embargo_minutes}m = {args.embargo_multiple:g}x horizon)"
    )
    for name, (lo, hi) in bounds.items():
        print(f"[split]   {name}: {lo.isoformat()} .. {hi.isoformat()}")

    Xtr, ytr = X[split.train], y[split.train]
    Xva, yva = X[split.valid], y[split.valid]
    Xte, yte = X[split.test], y[split.test]

    std = models.Standardiser.fit(Xtr)
    flat_cols = std.degenerate(Xtr)
    if flat_cols:
        print(
            "[warn] constant in the training window, std floored: "
            + ", ".join(schema.MODEL_FEATURE_SET[i] for i in flat_cols)
        )
    Ztr, Zva, Zte = std.transform(Xtr), std.transform(Xva), std.transform(Xte)

    if args.family == "logistic":
        fit = models.fit_logistic(Ztr, ytr, l2=args.l2, seed=args.seed)
        family = schema.FAMILY_LOGISTIC
    else:
        fit = models.fit_stumps(
            Ztr, ytr, rounds=args.rounds, learning_rate=args.learning_rate, seed=args.seed
        )
        family = schema.FAMILY_STUMPS
    print(f"[fit] family={family} {rng_note} hyperparams={fit.hyperparams}")

    # Calibration is fitted on validation and nowhere else.
    Pva_raw = forward.softmax_matrix(fit.scores(Zva))
    temperature = models.fit_temperature(Pva_raw, yva)
    Pva = models.apply_temperature(Pva_raw, temperature)
    val_ece, val_max_dev = metrics.expected_calibration_error(Pva, yva)
    raw_ece, _ = metrics.expected_calibration_error(Pva_raw, yva)
    print(
        f"[calibration] temperature={temperature:.6f} "
        f"validation ECE {raw_ece:.4f} -> {val_ece:.4f}"
    )

    artifact = build_artifact(
        args=args,
        ds=ds,
        family=family,
        fit=fit,
        std=std,
        temperature=temperature,
        val_ece=val_ece,
        val_max_dev=val_max_dev,
        bounds=bounds,
        samples=int(split.train.size + split.valid.size + split.test.size),
        contract_note=contract_note,
    )

    # Evaluate through the artifact, not through the in-memory fit. Rounding and
    # serialisation are part of the deployed model; scoring the unrounded fit
    # would report a model that does not exist.
    art = forward.Artifact(artifact)
    Pte = forward.predict_matrix(art, Xte)
    test = metrics.summarise(Pte, yte, regimes=ds.regimes()[split.test])
    Ptr = forward.predict_matrix(art, Xtr)
    train_summary = metrics.summarise(Ptr, ytr)
    record_test_report(artifact, test)
    artifact["metadata"]["notes"] = build_notes(test, train_summary, contract_note)

    problems = check_report_card(artifact)
    if problems:
        print("[error] the artifact would not load; refusing to write it:")
        for p in problems:
            print(f"[error]   {p}")
        return 1

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / f"{args.model_id}-{artifact['version']}.json"
    payload = json.dumps(artifact, indent=2, allow_nan=False) + "\n"
    path.write_text(payload, encoding="utf-8")
    digest = hashlib.sha256(payload.encode("utf-8")).hexdigest()

    _self_check(art, Xte[: min(64, len(Xte))], Pte[: min(64, len(Pte))])
    _write_sidecar(out_dir, args, artifact, path, digest, test, train_summary)
    _warn_about_siblings(out_dir, args.model_id, path)

    report(test, train_summary, temperature, val_ece)
    report_provisional_health(artifact)
    print(f"[artifact] {path}  sha256 {digest[:16]}")
    return 0


# --- artifact construction --------------------------------------------------


def build_artifact(
    *,
    args,
    ds: dataset.Dataset,
    family: str,
    fit,
    std: models.Standardiser,
    temperature: float,
    val_ece: float,
    val_max_dev: float,
    bounds: dict,
    samples: int,
    contract_note: str,
) -> dict:
    """Assemble the JSON object, in the field order of mlinfer.Artifact."""
    version = make_version(samples, ds.data_digest)

    art: dict = {
        "schema_version": schema.ARTIFACT_SCHEMA_VERSION,
        "model_id": args.model_id,
        "version": version,
        "family": family,
        "horizon": ds.horizon,
        "flat_band_bps": _round(ds.flat_band_bps),
        "features": list(schema.MODEL_FEATURE_SET),
        "mean": _round_all(std.mean),
        "std": _round_all(std.std),
    }
    if family == schema.FAMILY_LOGISTIC:
        art["coefficients"] = [_round_all(row) for row in fit.coefficients]
        art["intercepts"] = _round_all(fit.intercepts)
    else:
        art["stumps"] = [
            [
                {
                    "f": int(s["f"]),
                    "t": _round(s["t"]),
                    "l": _round(s["l"]),
                    "r": _round(s["r"]),
                }
                for s in group
            ]
            for group in fit.stumps
        ]
        art["base_score"] = _round_all(fit.base_score)

    art["calibration"] = {
        "method": "temperature",
        "temperature": _round(temperature),
        "ece": _round(val_ece),
        "max_deviation": _round(val_max_dev),
    }

    hyper = {k: _round(v) for k, v in fit.hyperparams.items()}
    hyper.update(
        {
            "seed": float(args.seed),
            "train_frac": _round(args.train_frac),
            "valid_frac": _round(args.valid_frac),
            "embargo_multiple": _round(args.embargo_multiple),
        }
    )
    art["metadata"] = {
        "trained_at": _trained_at(),
        "train_start": _rfc3339(bounds["train"][0]),
        "train_end": _rfc3339(bounds["train"][1]),
        "valid_start": _rfc3339(bounds["valid"][0]),
        "valid_end": _rfc3339(bounds["valid"][1]),
        "test_start": _rfc3339(bounds["test"][0]),
        "test_end": _rfc3339(bounds["test"][1]),
        "samples": samples,
        # The report card. Placeholders here only so the JSON key order matches
        # mlinfer.Metadata; record_test_report() below fills them from the test
        # split once the serialised artifact has been scored. An artifact that
        # reached disk still holding these zeros would be rejected by
        # Artifact.Validate, which is the behaviour we want if that ever happens.
        "test_samples": 0,
        "test_accuracy": 0.0,
        "test_base_rate": 0.0,
        "test_brier": 0.0,
        "training_commit": _git_commit(),
        "hyperparameters": hyper,
        "notes": "",
        "generator": GENERATOR,
    }
    return art


def record_test_report(artifact: dict, test: dict) -> dict:
    """Stamp the held-out report card onto ``metadata``.

    The numbers come from the test split — the one the fit never saw and the
    calibrator never saw either. Using validation here would be the same mistake
    as reporting training accuracy: the temperature was chosen on validation, so
    a validation score is a score of a decision, not of a model.

    The runtime reads these four fields directly. ``Artifact.Validate`` refuses
    to load an artifact missing any of the first three, and
    ``Artifact.ProvisionalHealth`` marks the model unhealthy — after which the
    risk engine blocks it — when ``test_accuracy <= test_base_rate``. So this is
    not bookkeeping: it is the model stating the bar it has to clear and its own
    score against it, in the same file, where neither can be quoted without the
    other.
    """
    md = artifact["metadata"]
    md["test_samples"] = int(test["n"])
    md["test_accuracy"] = _round(test["accuracy"])
    md["test_base_rate"] = _round(test["base_rate"])
    md["test_brier"] = _round(test["brier"])
    return md


def check_report_card(artifact: dict) -> list[str]:
    """Mirror the report-card half of ``mlinfer.Artifact.Validate``.

    Better to fail here, naming the field, than to write a file that the Go
    loader rejects at start-up — a directory holding an unloadable artifact makes
    the runtime log a load failure on every boot and serve the rule prior
    forever.
    """
    md = artifact["metadata"]
    problems = []
    if md.get("test_samples", 0) <= 0:
        problems.append("metadata.test_samples must be positive")
    acc = md.get("test_accuracy", 0.0)
    if not 0 < acc <= 1:
        problems.append(f"metadata.test_accuracy must be in (0, 1], got {acc!r}")
    base = md.get("test_base_rate", 0.0)
    if not 0 < base <= 1:
        problems.append(f"metadata.test_base_rate must be in (0, 1], got {base!r}")
    cal = artifact["calibration"]
    for key in ("ece", "max_deviation"):
        v = cal.get(key)
        if v is None or not 0 <= v <= 1:
            problems.append(f"calibration.{key} must be a proportion, got {v!r}")
    return problems


def provisional_health_verdict(artifact: dict) -> tuple[bool, list[str]]:
    """Predict what ``mlinfer.Artifact.ProvisionalHealth`` will decide.

    Reproduced here so the trainer can say, at the moment it writes the file,
    whether the runtime will serve this model or block it. The thresholds are the
    Go ones: accuracy must strictly beat the base rate, and the validation
    maximum calibration deviation must not exceed 0.10.
    """
    md = artifact["metadata"]
    issues = []
    if md["test_accuracy"] <= md["test_base_rate"]:
        issues.append(
            f"offline test accuracy {md['test_accuracy']:.4f} does not beat the "
            f"base rate {md['test_base_rate']:.4f}: the model is no better than a "
            "constant guess"
        )
    max_dev = artifact["calibration"].get("max_deviation", 0.0)
    if max_dev > 0.10:
        issues.append(
            f"offline maximum calibration deviation {max_dev:.4f} exceeds the "
            "0.10 promotion gate"
        )
    return (not issues), issues


def make_version(samples: int, data_digest: str) -> str:
    """``v1.<samples>.<short data hash>``, zero-padded so it sorts correctly.

    ``mlinfer.LoadDir`` promotes the lexicographically greatest version of a
    model id to active. Without the padding, an artifact fitted on 9,000 rows
    would outrank one fitted on 100,000, which is the wrong way round and is the
    kind of bug that only shows up in production.
    """
    return f"v1.{samples:09d}.{data_digest[:8]}"


def _round(v) -> float:
    """Round to SIGNIFICANT_DIGITS significant figures, returning a plain float."""
    x = float(v)
    if x == 0.0 or not math.isfinite(x):
        return 0.0 if x == 0.0 else x
    mag = math.floor(math.log10(abs(x)))
    return round(x, SIGNIFICANT_DIGITS - 1 - mag)


def _round_all(vs) -> list[float]:
    return [_round(v) for v in np.asarray(vs, dtype=np.float64).ravel()]


def _rfc3339(ts) -> str:
    return ts.tz_convert("UTC").strftime("%Y-%m-%dT%H:%M:%SZ")


def _trained_at() -> str:
    """Wall clock, or SOURCE_DATE_EPOCH when a reproducible build asks for it.

    This is the one field permitted to differ between two runs on the same data.
    """
    epoch = os.environ.get("SOURCE_DATE_EPOCH")
    if epoch and epoch.strip().isdigit():
        when = datetime.fromtimestamp(int(epoch), tz=timezone.utc)
    else:
        when = datetime.now(timezone.utc)
    return when.strftime("%Y-%m-%dT%H:%M:%SZ")


def _git_commit() -> str:
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=Path(__file__).resolve().parents[2],
            capture_output=True,
            text=True,
            timeout=5,
        )
        return out.stdout.strip() if out.returncode == 0 else ""
    except (OSError, subprocess.SubprocessError):
        return ""


# --- checks and reporting ---------------------------------------------------


def _self_check(art: forward.Artifact, X: np.ndarray, P: np.ndarray) -> None:
    """Assert the scalar and vectorised forward passes agree on the artifact.

    Cheap insurance against a serialisation bug: the numbers reported below are
    produced by the fast path, and only the slow path is what the Go parity test
    compares against.
    """
    if len(X) == 0:
        return
    worst = 0.0
    for i in range(len(X)):
        got = np.asarray(forward.predict_vector(art, X[i]))
        worst = max(worst, float(np.abs(got - P[i]).max()))
    if worst > 1e-9:
        raise AssertionError(
            f"scalar and vectorised forward passes disagree by {worst:g}; "
            "the artifact does not describe the model that was scored"
        )
    print(f"[check] scalar/vectorised forward passes agree to {worst:.2e}")


def build_notes(test: dict, train: dict, contract_note: str) -> str:
    verdict = (
        f"test accuracy {test['accuracy']:.4f} vs base rate {test['base_rate']:.4f} "
        f"({test['lift_over_base_rate']:+.4f})"
    )
    if not test["beats_base_rate"]:
        verdict += " - DOES NOT BEAT THE BASE RATE"
    return (
        f"{verdict}; train accuracy {train['accuracy']:.4f}; "
        f"test ECE {test['ece']:.4f}; {contract_note}. "
        "Educational research software: probabilistic output, no guarantees, "
        "no real-money orders."
    )


def report(test: dict, train: dict, temperature: float, val_ece: float) -> None:
    lift = test["lift_over_base_rate"]
    print("")
    print("  held-out test split (never seen by the fit or the calibrator)")
    print(f"    samples            {test['n']}")
    print(f"    accuracy           {test['accuracy']:.4f}")
    print(
        f"    base rate          {test['base_rate']:.4f}  "
        f"(always predict {test['majority_class']})"
    )
    print(f"    lift               {lift:+.4f}")
    print(f"    brier              {test['brier']:.4f}")
    print(
        f"    log loss           {test['log_loss']:.4f}  "
        f"(uniform {test['log_loss_uniform']:.4f}, class prior "
        f"{test['log_loss_class_prior']:.4f})"
    )
    print(f"    ECE                {test['ece']:.4f}  max dev {test['max_calibration_deviation']:.4f}")
    print(f"    train accuracy     {train['accuracy']:.4f}")
    print(f"    temperature        {temperature:.4f}  (validation ECE {val_ece:.4f})")
    for regime, row in (test.get("per_regime") or {}).items():
        print(
            f"    regime {regime:<16} n={row['n']:>7}  acc {row['accuracy']:.4f}  "
            f"base {row['base_rate']:.4f}  lift {row['lift']:+.4f}"
        )
    print("")
    if lift <= 0:
        print(
            "  VERDICT: the model does not beat the base rate on the held-out\n"
            "  split. It is written to disk so the pipeline stays exercised and\n"
            "  the number stays visible, but it carries no demonstrated\n"
            "  directional edge. Do not read the argmax as a forecast."
        )
        if test["beats_class_prior_log_loss"]:
            print(
                "  It does beat the constant class-prior predictor on log loss,\n"
                "  which means the probabilities carry a little information even\n"
                "  though the argmax does not. That is a calibration result, not\n"
                "  a trading result."
            )
    elif lift < 0.01:
        print(
            f"  VERDICT: {lift:+.4f} over the base rate. That is inside the range\n"
            "  a resample of this sample would move, so treat it as no\n"
            "  demonstrated edge rather than a small one."
        )
    else:
        print(
            f"  VERDICT: {lift:+.4f} over the base rate on held-out data. Still\n"
            "  educational output on simulated history, not a trading edge."
        )
    print("")


def _health_block(artifact: dict) -> dict:
    healthy, issues = provisional_health_verdict(artifact)
    return {
        "healthy": healthy,
        "issues": issues,
        "note": (
            "What mlinfer.Artifact.ProvisionalHealth will decide at load time. "
            "An unhealthy model is blocked by the risk engine, not served."
        ),
    }


def report_provisional_health(artifact: dict) -> None:
    """Say what the runtime will do with this artifact before it is ever loaded."""
    md = artifact["metadata"]
    healthy, issues = provisional_health_verdict(artifact)
    print("  report card carried in the artifact (read by mlinfer at load time)")
    print(f"    metadata.test_samples   {md['test_samples']}")
    print(f"    metadata.test_accuracy  {md['test_accuracy']:.6f}")
    print(f"    metadata.test_base_rate {md['test_base_rate']:.6f}")
    print(f"    metadata.test_brier     {md['test_brier']:.6f}")
    print(
        f"    calibration.ece         {artifact['calibration']['ece']:.6f}  "
        f"max_deviation {artifact['calibration']['max_deviation']:.6f} (validation)"
    )
    if healthy:
        print(
            "    ProvisionalHealth: HEALTHY — the model may serve on this offline\n"
            "    evidence until the first live evaluation sweep replaces it."
        )
    else:
        print("    ProvisionalHealth: UNHEALTHY — the risk engine will block it:")
        for issue in issues:
            print(f"      - {issue}")
    print("")


def _write_sidecar(
    out_dir: Path, args, artifact: dict, path: Path, digest: str, test: dict, train: dict
) -> None:
    """Write the human-facing pointer to the newest artifact.

    Deliberately named ``*.meta.json``: ``mlinfer.LoadDir`` skips that suffix, so
    the pointer cannot itself be loaded as a second copy of the model and end up
    competing with it for the active slot. The Go registry does not want a
    ``latest.json``; it derives "latest" from the version string.
    """
    meta = {
        "model_id": args.model_id,
        "version": artifact["version"],
        "artifact": path.name,
        "sha256": digest,
        "family": artifact["family"],
        "horizon": artifact["horizon"],
        "generator": GENERATOR,
        "trained_at": artifact["metadata"]["trained_at"],
        "report_card": {
            "test_samples": artifact["metadata"]["test_samples"],
            "test_accuracy": artifact["metadata"]["test_accuracy"],
            "test_base_rate": artifact["metadata"]["test_base_rate"],
            "test_brier": artifact["metadata"]["test_brier"],
            "validation_ece": artifact["calibration"]["ece"],
            "validation_max_deviation": artifact["calibration"]["max_deviation"],
        },
        "provisional_health": _health_block(artifact),
        "test": test,
        "train": train,
        "note": (
            "Pointer only. mlinfer.LoadDir ignores *.meta.json and selects the "
            "lexicographically greatest version for each model_id."
        ),
    }
    (out_dir / f"{args.model_id}-latest.meta.json").write_text(
        json.dumps(meta, indent=2) + "\n", encoding="utf-8"
    )


def _warn_about_siblings(out_dir: Path, model_id: str, written: Path) -> None:
    """Say so when another artifact for this model id will compete for active."""
    others = sorted(
        p.name
        for p in out_dir.glob(f"{model_id}-*.json")
        if p.name != written.name and not p.name.endswith((".meta.json", ".golden.json"))
    )
    if others:
        print(
            f"[warn] {out_dir} holds {len(others)} other artifact(s) for "
            f"{model_id}: {others}. mlinfer.LoadDir activates the greatest "
            "version string and registers the rest as shadows."
        )


# --- CLI --------------------------------------------------------------------


def parse_args(argv: list[str] | None):
    p = argparse.ArgumentParser(
        description="Fit the QuantOS 3-class direction model.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument("--data", required=True, help="feature CSV from `make export-features`")
    p.add_argument("--out", required=True, help="artifact directory (config predict.artifact_dir)")
    p.add_argument("--model-id", required=True, help="model id, e.g. direction-3class")
    p.add_argument("--horizon", required=True, choices=schema.DEFAULT_HORIZONS)
    p.add_argument("--family", default="logistic", choices=["logistic", "stumps"])
    p.add_argument("--seed", type=int, default=20260827, help="determinism seed")
    p.add_argument(
        "--flat-band-bps",
        type=float,
        default=15.0,
        help="declared FLAT half-width; cross-checked against the labels",
    )
    p.add_argument("--train-frac", type=float, default=0.6)
    p.add_argument("--valid-frac", type=float, default=0.2)
    p.add_argument(
        "--embargo-multiple",
        type=float,
        default=2.0,
        help="split gap as a multiple of the horizon; below 2 is rejected",
    )
    p.add_argument("--l2", type=float, default=1.0, help="logistic L2 penalty")
    p.add_argument("--rounds", type=int, default=60, help="boosting rounds (stumps)")
    p.add_argument("--learning-rate", type=float, default=0.1, help="shrinkage (stumps)")
    return p.parse_args(argv)


if __name__ == "__main__":
    raise SystemExit(main())
