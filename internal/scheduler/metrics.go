package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var deliveries = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fitbot_deliveries_total",
	Help: "Scheduled texts, by kind and result.",
}, []string{"kind", "result"})
