package zoneawareness

import (
	"github.com/coredns/coredns/plugin"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// reorderedQueriesCount exports a prometheus metric that is incremented every time a query's response is re-ordered.
var reorderedQueriesCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: plugin.Namespace,
	Subsystem: pluginName,
	Name:      "reordered_queries_total",
	Help:      "Total number of DNS queries that had their responses reordered by the zoneawareness plugin.",
}, []string{"server"})

// reorderCount exports a prometheus metric that is incremented by the number of responses that is re-ordered by the zoneawareness plugin.
var reorderCount = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: plugin.Namespace,
	Subsystem: pluginName,
	Name:      "reorder_count_total",
	Help:      "Number of records that was reordered by the zoneawareness plugin",
}, []string{"server"})

// reorderLatency is used to track the time spent to reorder DNS responses
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
