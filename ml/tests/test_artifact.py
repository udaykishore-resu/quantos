"""Artifact schema validity and trainer determinism.

The checks here mirror ``mlinfer.Artifact.Validate`` field for field. Running
them in Python is not a substitute for the Go loader — ``test_go_parity`` makes
the Go loader run for real — but it turns a schema mistake into a fast, local
failure with a message that names the field, instead of a start-up refusal
three commands later.
"""

from __future__ import annotations

import json

import numpy as np
import pytest

from ml.features import schema
from ml.inference import forward
from ml.training import train as trainer


@pytest.fixture(scope="module")
def art(trained_artifact) -> dict:
    return json.loads(trained_artifact.read_text())


def test_required_scalar_fields(art):
    assert art["schema_version"] == schema.ARTIFACT_SCHEMA_VERSION
    assert art["model_id"] and art["version"]
    assert art["family"] in (schema.FAMILY_LOGISTIC, schema.FAMILY_STUMPS)
    assert art["horizon"] in schema.DEFAULT_HORIZONS
    assert art["flat_band_bps"] > 0, "without a band the three classes are undefined"


def test_feature_block_matches_the_contract(art):
    assert art["features"] == schema.MODEL_FEATURE_SET
    n = len(art["features"])
    assert len(art["mean"]) == n
    assert len(art["std"]) == n
    assert all(s > 0 for s in art["std"]), "Validate() rejects a non-positive std"


def test_logistic_shapes(art):
    assert art["family"] == schema.FAMILY_LOGISTIC
    assert len(art["coefficients"]) == len(schema.ALL_OUTCOMES)
    assert all(len(row) == len(art["features"]) for row in art["coefficients"])
    assert len(art["intercepts"]) == len(schema.ALL_OUTCOMES)
    assert "stumps" not in art and "base_score" not in art


def test_stumps_shapes(stumps_artifact):
    art = json.loads(stumps_artifact.read_text())
    assert art["family"] == schema.FAMILY_STUMPS
    assert len(art["stumps"]) == len(schema.ALL_OUTCOMES)
    assert len(art["base_score"]) == len(schema.ALL_OUTCOMES)
    n = len(art["features"])
    for group in art["stumps"]:
        for s in group:
            assert 0 <= s["f"] < n, "a stump feature index out of range fails Validate"
            assert set(s) == {"f", "t", "l", "r"}
    assert "coefficients" not in art and "intercepts" not in art


def test_calibration_block_is_one_go_understands(art):
    cal = art["calibration"]
    assert cal["method"] in ("none", "temperature", "vector")
    if cal["method"] == "temperature":
        assert cal["temperature"] > 0
    assert 0 <= cal["ece"] <= 1
    assert 0 <= cal["max_deviation"] <= 1


def test_calibration_deviation_is_present_because_health_reads_it(art):
    """``ProvisionalHealth`` marks a model unhealthy when ``max_deviation`` is
    above 0.10. A zero written because nothing measured it is indistinguishable
    from a zero meaning perfect calibration, and would wave a miscalibrated model
    straight through the gate."""
    cal = art["calibration"]
    assert "max_deviation" in cal and "ece" in cal
    assert cal["max_deviation"] > 0, (
        "an exactly-zero maximum bin deviation over 15 bins means the field was "
        "never populated, not that calibration is perfect"
    )
    assert cal["ece"] > 0


def test_metadata_identifies_the_trainer(art):
    md = art["metadata"]
    assert md["generator"] == trainer.GENERATOR, (
        "a fitted model must never be mistakeable for a synthetic bootstrap one"
    )
    assert md["samples"] > 0
    assert md["train_start"] < md["train_end"] < md["valid_start"]
    assert md["valid_end"] < md["test_start"] < md["test_end"]
    assert md["notes"], "the notes carry the accuracy-vs-base-rate verdict"
    assert "base rate" in md["notes"]


# --- the held-out report card ------------------------------------------------
#
# mlinfer.Artifact.Validate refuses to load an artifact that omits test_samples,
# test_accuracy or test_base_rate, or whose accuracy and base rate are not
# proportions in (0, 1]. An artifact missing them is not a degraded artifact; it
# is an unloadable one, and the runtime then serves the deterministic rule prior
# for the life of the process. These tests encode that contract.

REPORT_CARD_FIELDS = ("test_samples", "test_accuracy", "test_base_rate")


@pytest.mark.parametrize("field", REPORT_CARD_FIELDS)
def test_report_card_field_is_present(art, field):
    assert field in art["metadata"], (
        f"metadata.{field} is required by Artifact.Validate; without it the "
        "artifact cannot be loaded at all"
    )


def test_report_card_ranges_match_what_validate_accepts(art):
    md = art["metadata"]
    assert isinstance(md["test_samples"], int) and md["test_samples"] > 0
    assert 0 < md["test_accuracy"] <= 1, (
        "Validate requires a proportion in (0, 1]; a percentage or a count here "
        "makes the artifact unloadable"
    )
    assert 0 < md["test_base_rate"] <= 1
    # Brier is optional in the schema but meaningless outside [0, 2] for three
    # classes, and ProvisionalHealth copies it onto the health record verbatim.
    assert 0 <= md["test_brier"] <= 2


def test_report_card_comes_from_the_test_split_and_no_other(art, synthetic_csv):
    """Recompute the held-out evaluation from the shipped artifact and require
    the recorded numbers to match.

    This is the test that distinguishes "the fields are populated" from "the
    fields are true". Reporting validation numbers here would flatter the model
    by the amount the temperature fit gained on that split, and reporting train
    numbers would flatter it by the whole of the overfit. Both would still
    satisfy Validate, so only recomputation catches them.
    """
    from ml.evaluation import metrics
    from ml.features import dataset

    ds = dataset.load(synthetic_csv, horizon=art["horizon"], declared_flat_band_bps=art["flat_band_bps"])
    X, y, ts = ds.matrix(), ds.labels(), ds.timestamps()
    split = dataset.chronological_split(
        ts, horizon=art["horizon"], train_frac=0.6, valid_frac=0.2, embargo_multiple=2.0
    )
    a = forward.Artifact(art)
    md = art["metadata"]

    test = metrics.summarise(
        forward.predict_matrix(a, X[split.test]), y[split.test]
    )
    assert md["test_samples"] == test["n"]
    assert md["test_accuracy"] == pytest.approx(test["accuracy"], abs=1e-9)
    assert md["test_base_rate"] == pytest.approx(test["base_rate"], abs=1e-9)
    assert md["test_brier"] == pytest.approx(test["brier"], abs=1e-9)

    # And explicitly not the other two splits.
    for name, idx in (("valid", split.valid), ("train", split.train)):
        other = metrics.summarise(forward.predict_matrix(a, X[idx]), y[idx])
        assert md["test_samples"] != other["n"], (
            f"metadata.test_samples matches the {name} split, not the test split"
        )


def test_report_card_states_the_bar_alongside_the_score(art):
    """Accuracy without its base rate is not a measurement. The whole reason
    Validate demands both is that 0.42 in a three-class problem is a result or a
    failure depending entirely on a number that has to travel with it."""
    md = art["metadata"]
    assert md["test_base_rate"] >= 1 / len(schema.ALL_OUTCOMES) - 1e-9, (
        "the base rate is the largest class share, so it cannot be below 1/3"
    )
    assert f"{md['test_accuracy']:.4f}" in md["notes"]
    assert f"{md['test_base_rate']:.4f}" in md["notes"]


def test_trainer_rejects_a_report_card_go_would_reject(art):
    """The trainer mirrors Validate's report-card rules so a bad artifact never
    reaches disk. Each mutation below is one Validate would refuse."""
    import copy

    assert trainer.check_report_card(art) == []

    for field, bad, expect in (
        ("test_samples", 0, "test_samples"),
        ("test_accuracy", 0.0, "test_accuracy"),
        ("test_accuracy", 1.5, "test_accuracy"),
        ("test_base_rate", 0.0, "test_base_rate"),
        ("test_base_rate", 42.0, "test_base_rate"),
    ):
        mutated = copy.deepcopy(art)
        mutated["metadata"][field] = bad
        problems = trainer.check_report_card(mutated)
        assert problems, f"{field}={bad!r} must be rejected"
        assert any(expect in p for p in problems)

    for field in REPORT_CARD_FIELDS:
        mutated = copy.deepcopy(art)
        del mutated["metadata"][field]
        assert trainer.check_report_card(mutated), f"a missing {field} must be rejected"


def test_provisional_health_verdict_follows_the_artifact_it_is_given(art):
    """The verdict must be a function of the two recorded numbers and nothing
    else, so it agrees with what the runtime will decide at load time.

    Note this fixture is deliberately *not* asserted healthy. It is fitted on
    synthetic data whose top-class confidences are poorly calibrated in the tail
    bins, so it clears its base rate and still fails the 0.10 deviation gate —
    which is exactly the case the gate exists for, and a useful reminder that
    beating the base rate is necessary and not sufficient. The artifact actually
    shipped in ml/artifacts is asserted healthy in test_go_parity.
    """
    md = art["metadata"]
    healthy, issues = trainer.provisional_health_verdict(art)

    beats_base = md["test_accuracy"] > md["test_base_rate"]
    calibrated = art["calibration"]["max_deviation"] <= 0.10
    assert healthy == (beats_base and calibrated)
    assert bool(issues) != healthy

    # The fit itself must have learned something, whatever the calibration says.
    assert beats_base, (
        "the synthetic labels depend on the first feature, so a working fit "
        "clears the base rate; failing here means the harness is broken, not "
        "that the model is weak"
    )


def test_a_model_that_does_not_beat_its_base_rate_is_marked_unhealthy(art):
    """The gate has to bite, not just exist. A model at or below its base rate
    is blocked by the risk engine rather than served — including the exact-tie
    case, which ProvisionalHealth treats as failure because a model that merely
    equals a constant guess has demonstrated nothing."""
    import copy

    tie = copy.deepcopy(art)
    tie["metadata"]["test_accuracy"] = tie["metadata"]["test_base_rate"]
    healthy, issues = trainer.provisional_health_verdict(tie)
    assert not healthy and any("base rate" in i for i in issues)

    worse = copy.deepcopy(art)
    worse["metadata"]["test_accuracy"] = worse["metadata"]["test_base_rate"] / 2
    assert not trainer.provisional_health_verdict(worse)[0]

    miscalibrated = copy.deepcopy(art)
    miscalibrated["calibration"]["max_deviation"] = 0.101
    healthy, issues = trainer.provisional_health_verdict(miscalibrated)
    assert not healthy and any("calibration deviation" in i for i in issues)

    # 0.10 exactly is the gate value, not a violation.
    edge = copy.deepcopy(art)
    edge["calibration"]["max_deviation"] = 0.10
    assert trainer.provisional_health_verdict(edge)[0]


def test_no_nan_or_infinity_anywhere(trained_artifact):
    """Go's json.Unmarshal rejects NaN outright; catching it here names the field."""
    def walk(node, path="$"):
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, f"{path}.{k}")
        elif isinstance(node, list):
            for i, v in enumerate(node):
                walk(v, f"{path}[{i}]")
        elif isinstance(node, float):
            assert np.isfinite(node), f"{path} is not finite"

    walk(json.loads(trained_artifact.read_text()))


def test_version_is_derived_from_the_data(art):
    assert art["version"].startswith("v1.")
    _, samples, digest = art["version"].split(".")
    assert int(samples) == art["metadata"]["samples"]
    assert len(digest) == 8


def test_version_sorts_by_sample_count_not_lexicographically():
    """mlinfer.LoadDir promotes the greatest version string. Zero padding is what
    stops a 9,000-row artifact outranking a 100,000-row one."""
    small = trainer.make_version(9_000, "aaaaaaaa")
    large = trainer.make_version(100_000, "aaaaaaaa")
    assert small < large


def test_rerunning_the_trainer_reproduces_the_artifact_byte_for_byte(
    synthetic_csv, tmp_path
):
    """The artifact digest is the identity of a deployed model. An identity that
    changes when nothing changed is worthless, so only trained_at may differ."""
    outs = []
    for run in ("a", "b"):
        d = tmp_path / run
        assert trainer.main(
            [
                "--data", str(synthetic_csv), "--out", str(d),
                "--model-id", "determinism", "--horizon", "60m", "--seed", "20260827",
            ]
        ) == 0
        written = [p for p in d.glob("*.json") if not p.name.endswith(".meta.json")]
        assert len(written) == 1
        outs.append(written[0])

    assert outs[0].name == outs[1].name
    a, b = (json.loads(p.read_text()) for p in outs)
    a["metadata"].pop("trained_at")
    b["metadata"].pop("trained_at")
    assert a == b

    # And byte-identical once that single field is normalised.
    ta = json.loads(outs[0].read_text())["metadata"]["trained_at"]
    text_b = outs[1].read_text().replace(
        json.loads(outs[1].read_text())["metadata"]["trained_at"], ta
    )
    assert outs[0].read_text() == text_b


def test_a_different_seed_does_not_silently_change_the_fit(synthetic_csv, tmp_path):
    """Neither fitter draws a random number, so the seed is recorded and inert.
    If that ever stops being true this test says so rather than a user noticing
    two 'identical' models disagreeing."""
    versions = []
    for seed in ("1", "20260827"):
        d = tmp_path / f"seed{seed}"
        assert trainer.main(
            [
                "--data", str(synthetic_csv), "--out", str(d),
                "--model-id", "seeded", "--horizon", "60m", "--seed", seed,
            ]
        ) == 0
        p = [q for q in d.glob("*.json") if not q.name.endswith(".meta.json")][0]
        body = json.loads(p.read_text())
        versions.append(body["coefficients"])
    assert versions[0] == versions[1]


def test_the_sidecar_is_invisible_to_the_go_loader(trained_artifact):
    """LoadDir skips *.meta.json. If the pointer were loadable it would register
    as a second copy of the model and compete for the active slot."""
    sidecars = list(trained_artifact.parent.glob("*-latest.meta.json"))
    assert len(sidecars) == 1
    meta = json.loads(sidecars[0].read_text())
    assert meta["artifact"] == trained_artifact.name
    assert meta["sha256"] == __import__("hashlib").sha256(
        trained_artifact.read_bytes()
    ).hexdigest()


def test_forward_pass_produces_a_valid_distribution(trained_artifact):
    a = forward.Artifact.load(trained_artifact)
    rng = np.random.default_rng(3)
    for _ in range(50):
        vec = rng.normal(0, 5, len(a.features))
        p = forward.predict_vector(a, vec)
        assert len(p) == 3
        assert abs(sum(p) - 1.0) < 1e-9
        assert all(0 <= x <= 1 for x in p)


def test_extreme_inputs_are_clamped_not_extrapolated(trained_artifact):
    """mlinfer clamps standardised inputs to +/-6. A bad tick must not produce a
    more extreme prediction than a merely large one."""
    a = forward.Artifact.load(trained_artifact)
    mean = np.asarray(a.mean)
    std = np.asarray(a.std)
    big = mean + 6 * std
    absurd = mean + 10_000 * std
    assert np.allclose(
        forward.predict_vector(a, big), forward.predict_vector(a, absurd), atol=1e-12
    )
