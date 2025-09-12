package zoneawareness

import (
	"github.com/coredns/coredns/plugin"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// requestCount exports a prometheus metric that is incremented every time a response is re-ordered by the zoneawareness plugin.
var requestCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: plugin.Namespace,
	Subsystem: pluginName,
	Name:      "queries_count_total",
	Help:      "Number of queries that have been successfully processed by the zoneawareness plugin",
}, []string{"server"})

// reorderCount exports a prometheus metric that is incremented by the number of responses that is re-ordered by the zoneawareness plugin.
var reorderCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: plugin.Namespace,
	Subsystem: pluginName,
	Name:      "reorder_count_total",
	Help:      "Number of records that was reordered by the zoneawareness plugin",
}, []string{"server"})

// reorderLatency
var reorderLatency = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace:                   plugin.Namespace,
		Subsystem:                   pluginName,
		Name:                        "reorder_duration_seconds",
		Help:                        "Reorder latency in seconds.",
		Buckets:                     prometheus.DefBuckets,
		NativeHistogramBucketFactor: plugin.NativeHistogramBucketFactor,
	},
	[]string{"server"},
)

// func init() {
// 	metrics.Register(metrics.RegisterOpts{
// 		RequestLatency: &latencyAdapter{m: requestLatency},
// 	})
// }

// type latencyAdapter struct {
// 	m *prometheus.HistogramVec
// }

// func (l *latencyAdapter) Observe(_ context.Context, latency time.Duration) {
// 	l.m.WithLabelValues(verb, u.Host).Observe(latency.Seconds())
// }
