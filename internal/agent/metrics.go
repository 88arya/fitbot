package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	llmCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_llm_calls_total",
		Help: "LLM calls, by model and result.",
	}, []string{"model", "result"})
	llmLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "spotter_llm_latency_seconds",
		Help:    "LLM call latency.",
		Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 16, 32},
	}, []string{"model"})
	llmTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_llm_tokens_total",
		Help: "LLM tokens used, by model and kind (input or output).",
	}, []string{"model", "kind"})
	llmCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_llm_cost_usd_total",
		Help: "Estimated LLM spend in USD from configured token prices.",
	}, []string{"model"})
	toolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_tool_calls_total",
		Help: "Tool calls made by the LLM, by tool and outcome.",
	}, []string{"tool", "outcome"})
)
