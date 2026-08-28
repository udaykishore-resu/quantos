#!/usr/bin/env python3
"""Walk-forward evaluation of the direction model.

Usage (this is exactly what `make evaluate` runs):

    python3 ml/evaluation/walk_forward.py --data ml/data/features.csv --out ml/reports

A single train/test split answers "did this model work on this one week". It is
the wrong question. What matters is whether the fitting procedure produces a
model that works on data it has not seen, repeatedly, as the sample rolls
forward — and whether the answer is stable or is one lucky fold carrying four
bad ones.

Every fold refits from scratch: the standardiser, the coefficients and the
calibration temperature. Reusing any of them across folds would leak the future
into the past, which is the failure this whole file exists to avoid.
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from ml.evaluation import metrics  # noqa: E402
from ml.features import dataset, schema  # noqa: E402
from ml.inference import forward  # noqa: E402
from ml.training import models  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    contract_note = schema.verify_against_go_source()
    print(f"[contract] {contract_note}")

    ds = dataset.load(
        args.data, horizon=args.horizon, declared_flat_band_bps=args.flat_band_bps
    )
    X, y, ts = ds.matrix(), ds.labels(), ds.timestamps()
    regimes = ds.regimes()
    print(f"[data] {ds.n} rows, horizon {ds.horizon}, digest {ds.data_digest[:12]}")

    folds = dataset.walk_forward_folds(
        ts,
        horizon=args.horizon,
        n_folds=args.folds,
        anchored=not args.rolling,
        embargo_multiple=args.embargo_multiple,
    )

    fold_reports = []
    pooled_P: list[np.ndarray] = []
    pooled_y: list[np.ndarray] = []
    pooled_regimes: list[np.ndarray] = []

    for i, split in enumerate(folds, start=1):
        std = models.Standardiser.fit(X[split.train])
        Ztr = std.transform(X[split.train])
        Zva = std.transform(X[split.valid])
        Zte = std.transform(X[split.test])

        if args.family == "logistic":
            fit = models.fit_logistic(Ztr, y[split.train], l2=args.l2, seed=args.seed)
        else:
            fit = models.fit_stumps(
                Ztr,
                y[split.train],
                rounds=args.rounds,
                learning_rate=args.learning_rate,
                seed=args.seed,
            )

        # Calibrate on validation, score on test. In that order, always.
        temperature = models.fit_temperature(
            forward.softmax_matrix(fit.scores(Zva)), y[split.valid]
        )
        P = models.apply_temperature(forward.softmax_matrix(fit.scores(Zte)), temperature)

        summary = metrics.summarise(P, y[split.test], regimes=regimes[split.test])
        bounds = split.bounds(ts)
        summary.update(
            {
                "fold": i,
                "temperature": round(temperature, 6),
                "train_n": int(split.train.size),
                "valid_n": int(split.valid.size),
                "embargoed_n": int(split.embargoed),
                "train_start": _iso(bounds["train"][0]),
                "train_end": _iso(bounds["train"][1]),
                "test_start": _iso(bounds["test"][0]),
                "test_end": _iso(bounds["test"][1]),
                "reliability": metrics.reliability_table(P, y[split.test]),
            }
        )
        fold_reports.append(summary)
        pooled_P.append(P)
        pooled_y.append(y[split.test])
        pooled_regimes.append(regimes[split.test])
        print(
            f"[fold {i}] train={split.train.size:>7} test={split.test.size:>7}  "
            f"acc {summary['accuracy']:.4f} base {summary['base_rate']:.4f} "
            f"lift {summary['lift_over_base_rate']:+.4f}  brier {summary['brier']:.4f} "
            f"ece {summary['ece']:.4f}"
        )

    P_all = np.vstack(pooled_P)
    y_all = np.concatenate(pooled_y)
    r_all = np.concatenate(pooled_regimes)
    pooled = metrics.summarise(P_all, y_all, regimes=r_all)
    pooled["reliability"] = metrics.reliability_table(P_all, y_all)

    lifts = np.array([f["lift_over_base_rate"] for f in fold_reports])
    verdict = _verdict(lifts, pooled)

    report = {
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "generator": "quantos/ml/evaluation/walk_forward.py",
        "data": str(ds.source),
        "data_sha256": ds.data_digest,
        "rows": ds.n,
        "symbols": int(ds.frame["ticker"].nunique()),
        "horizon": ds.horizon,
        "flat_band_bps": ds.flat_band_bps,
        "family": args.family,
        "seed": args.seed,
        "scheme": "rolling" if args.rolling else "anchored",
        "folds": len(fold_reports),
        "embargo_minutes": folds[0].embargo_minutes,
        "contract": contract_note,
        "fold_summary": {
            "mean_accuracy": round(
                float(np.mean([f["accuracy"] for f in fold_reports])), 6
            ),
            "mean_base_rate": round(
                float(np.mean([f["base_rate"] for f in fold_reports])), 6
            ),
            "mean_lift": round(float(lifts.mean()), 6),
            "std_lift": round(float(lifts.std(ddof=1)) if len(lifts) > 1 else 0.0, 6),
            "folds_beating_base_rate": int((lifts > 0).sum()),
        },
        "pooled": pooled,
        "per_fold": fold_reports,
        "verdict": verdict,
        "disclaimer": (
            "Educational research output on simulated market history. "
            "Probabilistic, frequently wrong, not financial advice, and never "
            "connected to a real-money order path."
        ),
    }

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    stem = f"walk-forward-{args.horizon}-{args.family}"
    (out_dir / f"{stem}.json").write_text(
        json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8"
    )
    (out_dir / f"{stem}.md").write_text(render_markdown(report), encoding="utf-8")

    print("")
    print(verdict)
    print("")
    print(f"[report] {out_dir / (stem + '.json')}")
    print(f"[report] {out_dir / (stem + '.md')}")
    return 0


def _iso(ts: pd.Timestamp) -> str:
    return ts.tz_convert("UTC").strftime("%Y-%m-%dT%H:%M:%SZ")


def _verdict(lifts: np.ndarray, pooled: dict) -> str:
    """State the result in the terms a reader should act on.

    The bar is deliberately awkward: a mean lift smaller than its own dispersion
    across folds is not a result, and saying so here is cheaper than saying it
    after someone has built on it.
    """
    mean = float(lifts.mean())
    spread = float(lifts.std(ddof=1)) if len(lifts) > 1 else 0.0
    won = int((lifts > 0).sum())
    head = (
        f"VERDICT: mean lift over the base rate is {mean:+.4f} across "
        f"{len(lifts)} folds (fold-to-fold sd {spread:.4f}); "
        f"{won}/{len(lifts)} folds beat their own base rate."
    )
    if mean <= 0:
        body = (
            "The model does not beat a constant guess out of sample. Nothing "
            "here supports serving it as a directional forecast."
        )
    elif spread >= abs(mean):
        body = (
            "The dispersion across folds is at least as large as the mean, so "
            "this is consistent with no edge at all. Treat it as a working "
            "pipeline, not a result."
        )
    else:
        body = (
            "The lift is positive and smaller than its dispersion is large, "
            "which is the weakest form of a real result. It is measured on "
            "simulated history and is not evidence of a tradeable edge."
        )
    prob = (
        "Probabilistically it beats the constant class-prior predictor on log "
        f"loss ({pooled['log_loss']:.4f} vs {pooled['log_loss_class_prior']:.4f})."
        if pooled["beats_class_prior_log_loss"]
        else "It does not beat the constant class-prior predictor on log loss, "
        f"either ({pooled['log_loss']:.4f} vs {pooled['log_loss_class_prior']:.4f})."
    )
    return f"{head}\n{body}\n{prob}"


def render_markdown(r: dict) -> str:
    fs = r["fold_summary"]
    p = r["pooled"]
    lines = [
        f"# Walk-forward evaluation — {r['horizon']} direction model",
        "",
        f"Generated {r['generated_at']} by `{r['generator']}`.",
        "",
        "> Educational research output on simulated market history. Probabilistic,",
        "> frequently wrong, not financial advice, and never connected to a",
        "> real-money order path.",
        "",
        "## Setup",
        "",
        f"| | |",
        f"|---|---|",
        f"| data | `{r['data']}` |",
        f"| data sha256 | `{r['data_sha256'][:16]}` |",
        f"| rows / symbols | {r['rows']:,} / {r['symbols']} |",
        f"| horizon | {r['horizon']} |",
        f"| FLAT band | ±{r['flat_band_bps']:g} bps |",
        f"| family | {r['family']} |",
        f"| scheme | {r['scheme']}, {r['folds']} folds |",
        f"| embargo | {r['embargo_minutes']} minutes between fit and test |",
        f"| seed | {r['seed']} |",
        f"| contract | {r['contract']} |",
        "",
        "## Verdict",
        "",
    ]
    lines += ["> " + line for line in r["verdict"].splitlines()]
    lines += [
        "",
        "## Per fold",
        "",
        "Accuracy is meaningless without the base rate beside it, so the two are"
        " never separated in this table.",
        "",
        "| fold | train | test | window | accuracy | base rate | lift | Brier |"
        " log loss | ECE | T |",
        "|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for f in r["per_fold"]:
        lines.append(
            f"| {f['fold']} | {f['train_n']:,} | {f['n']:,} | "
            f"{f['test_start'][:16]}Z → {f['test_end'][:16]}Z | "
            f"{f['accuracy']:.4f} | {f['base_rate']:.4f} | "
            f"{f['lift_over_base_rate']:+.4f} | {f['brier']:.4f} | "
            f"{f['log_loss']:.4f} | {f['ece']:.4f} | {f['temperature']:.3f} |"
        )
    lines += [
        "",
        f"Mean accuracy {fs['mean_accuracy']:.4f} against a mean base rate of "
        f"{fs['mean_base_rate']:.4f} — a mean lift of {fs['mean_lift']:+.4f} with a "
        f"fold-to-fold standard deviation of {fs['std_lift']:.4f}. "
        f"{fs['folds_beating_base_rate']} of {r['folds']} folds beat their own base rate.",
        "",
        "## Pooled over all folds",
        "",
        "| metric | value | reference |",
        "|---|---:|---|",
        f"| samples | {p['n']:,} | |",
        f"| accuracy | {p['accuracy']:.4f} | base rate {p['base_rate']:.4f}"
        f" (always predict {p['majority_class']}) |",
        f"| lift | {p['lift_over_base_rate']:+.4f} | 0.0000 is no edge |",
        f"| Brier | {p['brier']:.4f} | 0 perfect, 2 worst |",
        f"| log loss | {p['log_loss']:.4f} | uniform {p['log_loss_uniform']:.4f},"
        f" class prior {p['log_loss_class_prior']:.4f} |",
        f"| ECE | {p['ece']:.4f} | max bin deviation {p['max_calibration_deviation']:.4f} |",
        "",
        "### Class balance",
        "",
        "| class | share of outcomes |",
        "|---|---:|",
    ]
    for cls, share in p["class_distribution"].items():
        lines.append(f"| {cls} | {share:.4f} |")

    lines += [
        "",
        "### Confusion matrix",
        "",
        "Rows are the realised outcome, columns the predicted one, both in "
        "`domain.AllOutcomes` order.",
        "",
        "| actual \\ predicted | " + " | ".join(schema.ALL_OUTCOMES) + " |",
        "|---|" + "---:|" * len(schema.ALL_OUTCOMES),
    ]
    for i, row in enumerate(p["confusion_actual_by_predicted"]):
        lines.append(
            f"| {schema.ALL_OUTCOMES[i]} | " + " | ".join(f"{v:,}" for v in row) + " |"
        )

    if p.get("per_regime"):
        lines += [
            "",
            "### Per regime",
            "",
            "A model can clear the overall base rate by being right in whichever"
            " regime dominates the sample. This is where that shows.",
            "",
            "| regime | n | accuracy | base rate | lift |",
            "|---|---:|---:|---:|---:|",
        ]
        for regime, row in p["per_regime"].items():
            lines.append(
                f"| {regime} | {row['n']:,} | {row['accuracy']:.4f} | "
                f"{row['base_rate']:.4f} | {row['lift']:+.4f} |"
            )

    lines += [
        "",
        "### Reliability",
        "",
        "Confidence is the probability assigned to the predicted class. If the "
        "model is calibrated, the two right-hand columns match; the risk engine "
        "thresholds on this number, so the gap is the part that matters.",
        "",
        "| confidence bin | n | mean confidence | empirical accuracy |",
        "|---|---:|---:|---:|",
    ]
    for b in p["reliability"]:
        lines.append(
            f"| {b['bin_low']:.2f}–{b['bin_high']:.2f} | {b['count']:,} | "
            f"{b['mean_confidence']:.4f} | {b['empirical_accuracy']:.4f} |"
        )
    lines.append("")
    return "\n".join(lines)


def parse_args(argv: list[str] | None):
    p = argparse.ArgumentParser(
        description="Walk-forward evaluation of the QuantOS direction model.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument("--data", required=True, help="feature CSV from `make export-features`")
    p.add_argument("--out", required=True, help="report directory, e.g. ml/reports")
    p.add_argument("--horizon", default="60m", choices=schema.DEFAULT_HORIZONS)
    p.add_argument("--family", default="logistic", choices=["logistic", "stumps"])
    p.add_argument("--folds", type=int, default=5)
    p.add_argument(
        "--rolling",
        action="store_true",
        help="use a fixed-width rolling window instead of an anchored expanding one",
    )
    p.add_argument("--embargo-multiple", type=float, default=2.0)
    p.add_argument("--flat-band-bps", type=float, default=15.0)
    p.add_argument("--seed", type=int, default=20260827)
    p.add_argument("--l2", type=float, default=1.0)
    p.add_argument("--rounds", type=int, default=60)
    p.add_argument("--learning-rate", type=float, default=0.1)
    return p.parse_args(argv)


if __name__ == "__main__":
    raise SystemExit(main())
