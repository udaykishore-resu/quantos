// Command parity runs feature vectors through the *production* Go inference
// path and prints the resulting distributions as JSON.
//
// It exists so that ml/tests can assert that the Python re-implementation in
// ml/inference computes the same function as internal/mlinfer, rather than
// assuming it. The two implementations are written from the same specification
// by the same hand, which is precisely the situation in which a shared
// misreading goes unnoticed; only running both and diffing the output catches
// it.
//
// This tool must never grow inference logic of its own. Everything it prints
// comes from mlinfer.Model.PredictVector, unmodified — the moment it
// reimplements a step, it stops being evidence.
//
// usage:
//
//	go run ./ml/tools/parity -artifact ml/artifacts/<file>.json < vectors.json
//
// stdin is a JSON array of feature vectors (each an array of float64, in
// artifact feature order). With -vectors N and no stdin, N deterministic
// vectors are generated instead, which keeps the test independent of any data
// file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/udaykishoreresu/quantos/internal/mlinfer"
)

type output struct {
	ModelID  string      `json:"model_id"`
	Version  string      `json:"version"`
	Digest   string      `json:"digest"`
	Family   string      `json:"family"`
	Features []string    `json:"features"`
	Vectors  [][]float64 `json:"vectors"`
	Probs    [][]float64 `json:"probs"`
}

func main() {
	artifactPath := flag.String("artifact", "", "path to the model artifact JSON")
	count := flag.Int("vectors", 0, "generate this many deterministic vectors instead of reading stdin")
	seed := flag.Int64("seed", 20260827, "seed for the generated vectors")
	flag.Parse()

	if *artifactPath == "" {
		fail("-artifact is required")
	}
	art, err := mlinfer.LoadArtifact(*artifactPath)
	if err != nil {
		// Loading goes through Validate, so a schema violation surfaces here
		// with the same message the platform would print at start-up.
		fail("%v", err)
	}
	model := mlinfer.NewModel(art)

	vectors, err := readVectors(*count, len(art.Features), *seed)
	if err != nil {
		fail("%v", err)
	}

	out := output{
		ModelID: art.ModelID, Version: art.Version, Digest: art.Digest,
		Family: string(art.Family), Features: art.Features, Vectors: vectors,
	}
	for i, v := range vectors {
		dist, _, err := model.PredictVector(v)
		if err != nil {
			fail("vector %d: %v", i, err)
		}
		out.Probs = append(out.Probs, []float64{dist.Up, dist.Flat, dist.Down})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fail("%v", err)
	}
}

// readVectors takes vectors from stdin, or synthesises them when asked.
//
// The synthetic generator is a plain linear congruential sequence rather than
// math/rand: its output must not change if the Go runtime's generator is ever
// reseeded or reimplemented, because the Python side reproduces the same
// sequence to build the same inputs.
func readVectors(count, dim int, seed int64) ([][]float64, error) {
	if count > 0 {
		vectors := make([][]float64, count)
		state := uint64(seed)
		for i := range vectors {
			v := make([]float64, dim)
			for j := range v {
				state = state*6364136223846793005 + 1442695040888963407
				// Spread over a range wide enough that some coordinates land
				// outside the +/-6 clamp, so the clamp itself is covered.
				u := float64(state>>11) / float64(uint64(1)<<53)
				v[j] = (u*2 - 1) * 12
			}
			vectors[i] = v
		}
		return vectors, nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	var vectors [][]float64
	if err := json.Unmarshal(data, &vectors); err != nil {
		return nil, fmt.Errorf("parse stdin as [][]float64: %w", err)
	}
	for i, v := range vectors {
		if len(v) != dim {
			return nil, fmt.Errorf("vector %d has %d entries, artifact declares %d", i, len(v), dim)
		}
		for j, x := range v {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, fmt.Errorf("vector %d[%d] is not finite", i, j)
			}
		}
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("no vectors supplied: pass -vectors N or pipe JSON on stdin")
	}
	return vectors, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "parity: "+format+"\n", args...)
	os.Exit(1)
}
