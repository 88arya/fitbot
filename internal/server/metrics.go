package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	smsMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fitbot_sms_messages_total",
		Help: "Texts received and sent, by direction and result.",
	}, []string{"direction", "result"})
	smsDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fitbot_sms_deliveries_total",
		Help: "Final delivery status of outbound texts, as reported by Telnyx.",
	}, []string{"status"})
	smsCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fitbot_sms_cost_usd_total",
		Help: "What Telnyx reported charging for texts, in USD.",
	}, []string{"direction"})
	webhookEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fitbot_webhook_events_total",
		Help: "Telnyx webhooks, by event type and outcome.",
	}, []string{"type", "outcome"})
)
