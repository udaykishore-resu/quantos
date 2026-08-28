package predict

import (
	"math"
	"testing"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/mlinfer"
)

func modelWith(accuracy, baseRate, auc float64) *mlinfer.Model {
	return mlinfer.NewModel(&mlinfer.Artifact{
		ModelID: "test", Version: "v1",
		Metadata: mlinfer.Metadata{
			TestSamples: 1000, TestAccuracy: accuracy, TestBaseRate: baseRate,
			TestDirectionalAUC: auc,
		},
	})
}

// TestAModelWithNoLiftEarnsNoWeight is the rule that stops an unmeasured or
// useless model from diluting the deterministic prior. Averaging a considered
// prior with a coin flip produces something worse than the prior alone, and
// nothing about the arithmetic makes that visible.
func TestAModelWithNoLiftEarnsNoWeight(t *testing.T) {
	cfg := DefaultConfig()
	e := &Engine{cfg: cfg}

	if w := e.effectiveBlendWeight(modelWith(0.39, 0.39, 0.6)); w != 0 {
		t.Fatalf("a model exactly at its base rate earned weight %.4f", w)
	}
	if w := e.effectiveBlendWeight(modelWith(0.30, 0.39, 0.6)); w != 0 {
		t.Fatalf("a model below its base rate earned weight %.4f", w)
	}
}

func TestBlendWeightScalesWithMeasuredLift(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BlendWeight = 0.6
	cfg.BlendReferenceLift = 0.05
	e := &Engine{cfg: cfg}

	half := e.effectiveBlendWeight(modelWith(0.415, 0.39, 0.6)) // lift 0.025 = half the reference
	if math.Abs(half-0.3) > 1e-9 {
		t.Fatalf("a model at half the reference lift earned %.4f, expected 0.30", half)
	}
	full := e.effectiveBlendWeight(modelWith(0.50, 0.39, 0.6)) // lift 0.11, past the reference
	if math.Abs(full-0.6) > 1e-9 {
		t.Fatalf("a model well past the reference lift earned %.4f, expected the configured 0.60", full)
	}
}

// TestDirectionIsNotTakenFromAnUnmeasuredModel covers the split that matters:
// a three-class model can be genuinely good at separating a quiet tape from a
// moving one and no better than chance at telling up from down. Taking its
// direction anyway is how a 0.50-AUC model ends up steering a signal.
func TestDirectionIsNotTakenFromAnUnmeasuredModel(t *testing.T) {
	cfg := DefaultConfig()
	e := &Engine{cfg: cfg}

	m := modelWith(0.45, 0.39, 0) // never measured
	if w := e.directionalWeight(m, 0.6); w != 0 {
		t.Fatalf("an unmeasured direction earned weight %.4f", w)
	}
	m = modelWith(0.45, 0.39, 0.49) // measured, and worse than a coin flip
	if w := e.directionalWeight(m, 0.6); w != 0 {
		t.Fatalf("a below-chance direction earned weight %.4f", w)
	}
	m = modelWith(0.45, 0.39, 0.53) // measured at the reference edge
	if w := e.directionalWeight(m, 0.6); math.Abs(w-0.6) > 1e-9 {
		t.Fatalf("a model at the reference AUC earned %.4f, expected the full 0.60", w)
	}
}

func TestBlendDirectionalKeepsThePriorsDirectionAtZeroSkill(t *testing.T) {
	prior := domain.NewDistribution(0.55, 0.30, 0.15)
	model := domain.NewDistribution(0.20, 0.60, 0.20) // flat-heavy, perfectly undirected

	out := blendDirectional(prior, model, 0.6, 0)

	// The FLAT mass moves towards the model...
	wantFlat := 0.4*prior.Flat + 0.6*model.Flat
	if math.Abs(out.Flat-wantFlat) > 1e-9 {
		t.Fatalf("flat mass %.6f, expected %.6f", out.Flat, wantFlat)
	}
	// ...while the up/down ratio is exactly the prior's.
	priorRatio := prior.Up / (prior.Up + prior.Down)
	outRatio := out.Up / (out.Up + out.Down)
	if math.Abs(outRatio-priorRatio) > 1e-9 {
		t.Fatalf("directional ratio moved from %.6f to %.6f despite zero measured skill",
			priorRatio, outRatio)
	}
	if math.Abs(out.Up+out.Flat+out.Down-1) > 1e-9 {
		t.Fatalf("the blended distribution does not sum to one: %+v", out)
	}
}

func TestBlendDirectionalMatchesAPlainBlendAtEqualWeights(t *testing.T) {
	prior := domain.NewDistribution(0.55, 0.30, 0.15)
	model := domain.NewDistribution(0.20, 0.60, 0.20)
	want := prior.Blend(model, 0.6)
	got := blendDirectional(prior, model, 0.6, 0.6)
	if math.Abs(got.Up-want.Up) > 1e-9 || math.Abs(got.Flat-want.Flat) > 1e-9 || math.Abs(got.Down-want.Down) > 1e-9 {
		t.Fatalf("split blend %+v differs from the plain blend %+v when the weights agree", got, want)
	}
}

func TestBlendDirectionalTakesDirectionWhenSkillIsMeasured(t *testing.T) {
	prior := domain.NewDistribution(0.50, 0.30, 0.20) // leans up
	model := domain.NewDistribution(0.15, 0.30, 0.55) // leans down, and has earned the right to
	out := blendDirectional(prior, model, 0.6, 0.6)
	if out.Down <= out.Up {
		t.Fatalf("a model with full directional weight did not move the direction: %+v", out)
	}
}

func TestConfidenceIsNotOneMinusUncertainty(t *testing.T) {
	// Guards a real defect: defining confidence as 1 - normalised entropy made
	// the spec's own worked example score 0.17, below every gate in the
	// platform, so no signal could ever be emitted.
	d := domain.NewDistribution(0.63, 0.21, 0.16)
	c := d.Confidence()
	if c < 0.40 || c > 0.50 {
		t.Fatalf("a 63/21/16 distribution has confidence %.4f; the gates are set for the "+
			"margin-over-uniform definition, which puts it near 0.445", c)
	}
	flat := domain.NewDistribution(1.0/3, 1.0/3, 1.0/3)
	if flat.Confidence() != 0 {
		t.Fatalf("a uniform distribution has confidence %.4f", flat.Confidence())
	}
	certain := domain.NewDistribution(1, 0, 0)
	if math.Abs(certain.Confidence()-1) > 1e-9 {
		t.Fatalf("a degenerate distribution has confidence %.4f", certain.Confidence())
	}
}
