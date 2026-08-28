"""Go/Python parity: the two implementations must compute the same function.

This is the test the rest of the tree exists to make possible. Everything else
checks that the trainer is internally consistent; only this one checks that the
model the trainer *thinks* it fitted is the model ``internal/mlinfer`` will
actually serve.

It runs the real Go inference path via ``ml/tools/parity`` and compares its
output against ``ml/inference/forward``. The comparison is not bit-exact by
design: Go's ``math.Exp`` and ``math.Pow`` are its own implementations, not
glibc's, and may differ in the last unit in the last place. The tolerance below
is tight enough that any real disagreement — a transposed row, a missed clamp,
a calibration applied in the wrong space — fails by many orders of magnitude,
and loose enough that a libm difference does not.
"""

from __future__ import annotations

import numpy as np
import pytest

from conftest import run_go_parity  # noqa: F401  (pytest puts ml/tests on sys.path)
from ml.features import schema
from ml.inference import forward

#: Absolute tolerance on a probability. Chosen against the observed libm gap,
#: which is ~1e-16; a genuine implementation divergence moves probabilities by
#: 1e-3 or more.
TOLERANCE = 1e-12

pytestmark = pytest.mark.usefixtures("go_available")


@pytest.fixture(autouse=True)
def _require_go(go_available):
    if not go_available:
        pytest.fail(
            "the Go toolchain is not on PATH, so Go/Python parity cannot be "
            "verified. This test must not be skipped quietly: an unverified "
            "parity claim is the thing it exists to prevent."
        )


def _compare(artifact, count=64):
    out = run_go_parity(artifact, count)
    assert out["features"] == schema.MODEL_FEATURE_SET
    art = forward.Artifact.load(artifact)

    worst = 0.0
    for vec, go_probs in zip(out["vectors"], out["probs"]):
        py = forward.predict_vector(art, vec)
        worst = max(worst, float(np.abs(np.asarray(py) - np.asarray(go_probs)).max()))
    return out, worst


def test_logistic_parity_against_go(trained_artifact):
    out, worst = _compare(trained_artifact)
    assert out["family"] == schema.FAMILY_LOGISTIC
    assert worst < TOLERANCE, f"Go and Python disagree by {worst:g}"


def test_stumps_parity_against_go(stumps_artifact):
    out, worst = _compare(stumps_artifact)
    assert out["family"] == schema.FAMILY_STUMPS
    assert worst < TOLERANCE, f"Go and Python disagree by {worst:g}"


def test_shipped_artifact_parity(shipped_artifact):
    """The artifact actually in ml/artifacts, if one has been trained."""
    if shipped_artifact is None:
        pytest.skip("no artifact in ml/artifacts; run `make train` first")
    _, worst = _compare(shipped_artifact, count=128)
    assert worst < TOLERANCE, f"Go and Python disagree by {worst:g}"


def test_go_computes_the_digest_python_records(shipped_artifact):
    """mlinfer stamps the artifact's sha256 onto every prediction for
    reproducibility; the sidecar must record the same one."""
    import hashlib
    import json

    if shipped_artifact is None:
        pytest.skip("no artifact in ml/artifacts")
    out = run_go_parity(shipped_artifact, 1)
    assert out["digest"] == hashlib.sha256(shipped_artifact.read_bytes()).hexdigest()

    sidecar = next(shipped_artifact.parent.glob("*-latest.meta.json"), None)
    if sidecar is not None and json.loads(sidecar.read_text())["artifact"] == (
        shipped_artifact.name
    ):
        assert json.loads(sidecar.read_text())["sha256"] == out["digest"]


def test_go_loader_enforces_the_schema(trained_artifact, tmp_path):
    """Corrupt one field and confirm the Go side refuses rather than serving.

    Parity is only worth having if the Go loader is strict; this proves the
    artifact is being validated, not merely parsed.
    """
    import json

    body = json.loads(trained_artifact.read_text())
    body["std"][3] = 0.0  # Validate() requires every std to be positive
    bad = tmp_path / "bad.json"
    bad.write_text(json.dumps(body))

    with pytest.raises(AssertionError) as err:
        run_go_parity(bad, 1)
    assert "std[3] must be positive" in str(err.value)


def test_go_loader_requires_the_held_out_report_card(trained_artifact, tmp_path):
    """Strip each required report-card field and confirm the real Go loader
    refuses the artifact.

    This is the contract that matters: an artifact without its own held-out
    numbers is not loaded at all, so the platform runs on the deterministic rule
    prior instead of on an unmeasured model. Asserting it against the Go binary
    rather than the Python mirror is the point — the Python mirror is a
    convenience, ``Validate`` is the authority.
    """
    import json

    for field, needle in (
        ("test_samples", "test_samples"),
        ("test_accuracy", "test_accuracy"),
        ("test_base_rate", "test_base_rate"),
    ):
        body = json.loads(trained_artifact.read_text())
        del body["metadata"][field]
        bad = tmp_path / f"missing-{field}.json"
        bad.write_text(json.dumps(body))
        with pytest.raises(AssertionError) as err:
            run_go_parity(bad, 1)
        assert needle in str(err.value), (
            f"the Go loader accepted an artifact with no metadata.{field}"
        )


def test_go_loader_rejects_a_report_card_out_of_range(trained_artifact, tmp_path):
    """A proportion that is not a proportion — a percentage, a count, a negative
    — is rejected rather than silently compared against the base rate."""
    import json

    for field, value in (
        ("test_accuracy", 41.9),
        ("test_accuracy", 0.0),
        ("test_base_rate", 39.2),
        ("test_base_rate", -0.1),
        ("test_samples", 0),
    ):
        body = json.loads(trained_artifact.read_text())
        body["metadata"][field] = value
        bad = tmp_path / f"range-{field}-{value}.json"
        bad.write_text(json.dumps(body))
        with pytest.raises(AssertionError) as err:
            run_go_parity(bad, 1)
        assert field in str(err.value)


def test_shipped_artifact_loads_and_would_be_served(shipped_artifact):
    """The artifact in ml/artifacts must pass the loader it will meet at
    start-up, and clear ProvisionalHealth once there. A directory holding an
    unloadable artifact makes the runtime log a load failure on every boot."""
    import json

    if shipped_artifact is None:
        pytest.skip("no artifact in ml/artifacts; run `make train` first")
    run_go_parity(shipped_artifact, 1)  # raises if Validate rejects it

    from ml.training import train as trainer

    body = json.loads(shipped_artifact.read_text())
    assert trainer.check_report_card(body) == []
    healthy, issues = trainer.provisional_health_verdict(body)
    assert healthy, f"the shipped artifact would be blocked by the risk engine: {issues}"


def test_vectorised_and_scalar_paths_agree(shipped_artifact, trained_artifact):
    """The fast path used for evaluation must match the path parity is proven on,
    or the reported metrics describe a different model from the deployed one."""
    artifact = shipped_artifact or trained_artifact
    art = forward.Artifact.load(artifact)
    rng = np.random.default_rng(20260827)
    X = rng.normal(0, 4, size=(256, len(art.features)))
    fast = forward.predict_matrix(art, X)
    for i in range(len(X)):
        slow = forward.predict_vector(art, X[i])
        assert np.abs(np.asarray(slow) - fast[i]).max() < 1e-9
