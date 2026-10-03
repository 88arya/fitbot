package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	smsMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_sms_messages_total",
		Help: "Texts received and sent, by direction and result.",
	}, []string{"direction", "result"})
	smsDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_sms_deliveries_total",
		Help: "Final delivery status of outbound texts, as reported by Telnyx.",
	}, []string{"status"})
	smsCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_sms_cost_usd_total",
		Help: "What Telnyx reported charging for texts, in USD.",
	}, []string{"direction"})
	webhookEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "spotter_webhook_events_total",
		Help: "Telnyx webhooks, by event type and outcome.",
	}, []string{"type", "outcome"})

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
)
