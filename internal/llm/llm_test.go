package llm

import (
	"math"
	"testing"
)

func TestCost(t *testing.T) {
	c := New("", "", "m", 0.75, 4.50)
	usd, ok := c.Cost(Usage{InputTokens: 43, OutputTokens: 14})
	if !ok || math.Abs(usd-0.00009525) > 1e-12 {
		t.Errorf("Cost = %v, %v", usd, ok)
	}

	if _, ok := New("", "", "m", 0, 0).Cost(Usage{InputTokens: 1}); ok {
		t.Error("unpriced model reported a cost")
	}
}
