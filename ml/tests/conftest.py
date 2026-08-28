"""Shared fixtures for the research tests.

The repository root goes on ``sys.path`` so ``ml.*`` imports resolve the same
way whether pytest is invoked from the root, from ``ml/`` or from CI.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

from ml.features import schema  # noqa: E402
from ml.training import models  # noqa: E402
from ml.training import train as trainer  # noqa: E402

#: Environment `go run` needs in this container: the module proxy is not
#: reachable, and every dependency is either vendored in go.sum or stdlib.
GO_ENV = {
    "GOPROXY": "direct",
    "GOSUMDB": "off",
    "GOPRIVATE": "*",
    "GOFLAGS": "-mod=mod",
}


@pytest.fixture(scope="session")
def repo_root() -> Path:
    return ROOT


@pytest.fixture(scope="session")
def go_available() -> bool:
    return shutil.which("go") is not None


@pytest.fixture(scope="session")
def synthetic_csv(tmp_path_factory) -> Path:
    """A small features CSV with the exact header the Go exporter writes.

    Synthetic rather than a slice of the real export: the tests must fail when
    the *contract* is broken, and a fixture generated from the contract itself
    is the only way to be sure a passing test means the contract holds. It also
    keeps the suite runnable when ml/data has not been exported.
    """
    rng = np.random.default_rng(20260827)
    # Enough one-minute bars to survive a 120-minute embargo in every one of the
    # five walk-forward folds, and few enough symbols that the suite stays fast.
    n_ticks, n_symbols = 3000, 4
    rows = []
    header = schema.expected_csv_header()
    start = np.datetime64("2026-01-24T21:00:00")
    for t in range(n_ticks):
        ts = np.datetime_as_string(start + np.timedelta64(t, "m"), unit="s") + "Z"
        for s in range(n_symbols):
            feats = rng.normal(0, 1, len(schema.MODEL_FEATURE_SET))
            # Give the label a weak, real dependence on the first feature so
            # that "the fit learned nothing" and "the harness is broken" are
            # distinguishable outcomes.
            drive = feats[0] * 12.0 + rng.normal(0, 20.0)
            rec = [ts, f"SYM{s}", f"{100 + s:.4f}", "RANGE"]
            rec += [f"{v:.8f}" for v in feats]
            for h in schema.DEFAULT_HORIZONS:
                r = drive * schema.horizon_minutes(h) / 60.0
                label = "FLAT"
                if r > 15:
                    label = "UP"
                elif r < -15:
                    label = "DOWN"
                rec += [f"{r:.4f}", label]
            rows.append(",".join(rec))

    path = tmp_path_factory.mktemp("features") / "features.csv"
    path.write_text(",".join(header) + "\n" + "\n".join(rows) + "\n", encoding="utf-8")
    return path


@pytest.fixture(scope="session")
def trained_artifact(synthetic_csv, tmp_path_factory) -> Path:
    """Run the real trainer end to end and return the artifact it wrote."""
    out = tmp_path_factory.mktemp("artifacts")
    rc = trainer.main(
        [
            "--data", str(synthetic_csv),
            "--out", str(out),
            "--model-id", "test-direction-3class",
            "--horizon", "60m",
            "--seed", "20260827",
        ]
    )
    assert rc == 0
    written = [
        p for p in out.glob("*.json") if not p.name.endswith(".meta.json")
    ]
    assert len(written) == 1, f"expected exactly one artifact, got {written}"
    return written[0]


@pytest.fixture(scope="session")
def stumps_artifact(synthetic_csv, tmp_path_factory) -> Path:
    """The same data fitted with the other family the Go runtime can serve."""
    out = tmp_path_factory.mktemp("artifacts-stumps")
    rc = trainer.main(
        [
            "--data", str(synthetic_csv),
            "--out", str(out),
            "--model-id", "test-direction-stumps",
            "--horizon", "60m",
            "--family", "stumps",
            "--rounds", "20",
            "--seed", "20260827",
        ]
    )
    assert rc == 0
    written = [p for p in out.glob("*.json") if not p.name.endswith(".meta.json")]
    assert len(written) == 1
    return written[0]


@pytest.fixture(scope="session")
def shipped_artifact() -> Path | None:
    """The artifact in ml/artifacts, when one has been trained."""
    art_dir = ROOT / "ml" / "artifacts"
    candidates = sorted(
        p for p in art_dir.glob("*.json") if not p.name.endswith(".meta.json")
    )
    return candidates[-1] if candidates else None


def run_go_parity(artifact: Path, count: int, seed: int = 20260827) -> dict:
    """Invoke the Go parity helper and return its decoded output."""
    import json
    import os

    env = dict(os.environ)
    env.update(GO_ENV)
    proc = subprocess.run(
        [
            "go", "run", "./ml/tools/parity",
            "-artifact", str(artifact),
            "-vectors", str(count),
            "-seed", str(seed),
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        env=env,
        timeout=600,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"go run ./ml/tools/parity failed ({proc.returncode}):\n{proc.stderr}"
        )
    return json.loads(proc.stdout)


__all__ = ["ROOT", "GO_ENV", "run_go_parity", "models", "schema", "trainer"]
