package llm

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Mock is a deterministic provider used for local development, tests and CI.
//
// It is not a language model. It composes prose from the structured facts in
// the prompt, choosing phrasing deterministically from a hash of the input so
// that output varies between inputs but never between runs. Critically, it only
// ever emits numbers that appear in the prompt, which means the grounding
// validator's happy path is exercised on every test run rather than only when
// a real provider is configured.
type Mock struct {
	clock obs.Clock
	// Latency simulates provider delay, so timeout handling can be tested.
	Latency time.Duration
	// FailEvery makes every n-th call fail, for exercising the fallback path.
	FailEvery int
	calls     int
}

// NewMock returns a deterministic provider.
func NewMock(clock obs.Clock) *Mock {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return &Mock{clock: clock}
}

// Name implements Provider.
func (m *Mock) Name() string { return "mock" }

// Model implements Provider.
func (m *Mock) Model() string { return "quantos-deterministic-mock" }

var factLine = regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z][A-Za-z0-9 _/()%.-]*):\s*(.+)$`)

// Complete generates a deterministic narrative from the prompt's fact lines.
func (m *Mock) Complete(ctx context.Context, req Request) (Response, error) {
	m.calls++
	if m.FailEvery > 0 && m.calls%m.FailEvery == 0 {
		return Response{}, fmt.Errorf("%w: simulated failure", ErrUnavailable)
	}
	if m.Latency > 0 {
		select {
		case <-ctx.Done():
			return Response{}, ErrTimeout
		case <-time.After(m.Latency):
		}
	}
	select {
	case <-ctx.Done():
		return Response{}, ErrTimeout
	default:
	}

	prompt := ""
	for _, msg := range req.Messages {
		if msg.Role == RoleUser {
			prompt += msg.Content + "\n"
		}
	}
	facts := factLine.FindAllStringSubmatch(prompt, -1)

	h := fnv.New32a()
	_, _ = h.Write([]byte(prompt))
	seed := int(h.Sum32())

	openers := []string{
		"The structured inputs describe the following picture.",
		"Reading the supplied inputs in order:",
		"On the evidence provided:",
		"The record for this instrument reads as follows.",
	}
	closers := []string{
		"These are probabilities derived from the inputs above, not predictions of what will happen.",
		"Every figure above is taken directly from the structured record; none is inferred.",
		"The distribution expresses uncertainty rather than expectation, and the setup may fail.",
		"This is research output. It describes what the inputs say, not what an instrument will do.",
	}

	var b strings.Builder
	b.WriteString(openers[seed%len(openers)])
	b.WriteString("\n\n")

	shown := 0
	for _, f := range facts {
		key := strings.TrimSpace(f[1])
		val := strings.TrimSpace(f[2])
		if val == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("%s: %s.\n", key, val))
		shown++
		if shown >= 12 {
			break
		}
	}
	if shown == 0 {
		b.WriteString("No structured facts were supplied, so there is nothing to summarise.\n")
	}
	b.WriteString("\n")
	b.WriteString(closers[(seed/7)%len(closers)])

	text := b.String()
	return Response{
		Text:         text,
		Model:        m.Model(),
		Provider:     m.Name(),
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(text) / 4,
		StopReason:   "end_turn",
	}, nil
}
