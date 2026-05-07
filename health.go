package main

import (
	"github.com/docker/docker/api/types/swarm"
	"github.com/prometheus/client_golang/prometheus"
)

var healthRatioGauge *prometheus.GaugeVec

func configureHealthGauge() {
	labels := append([]string{
		"stack",
		"service",
		"service_mode",
	}, customLabels...)

	healthRatioGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarm_service_health_ratio",
		Help: "Health ratio: 1=healthy or completed, 0=down, 0<x<1=degraded or in-progress",
	}, sanitizeLabelNames(labels))
	prometheus.MustRegister(healthRatioGauge)
}

func updateHealthGauge(sctr serviceCounter) {
	effectiveMu.RLock()
	defer effectiveMu.RUnlock()

	emitted := make(map[string]bool)

	for _, versions := range sctr {
		for _, tctr := range versions {
			serviceName := tctr.labels["service"]
			entry, ok := effectiveDesiredCache[serviceName]
			if !ok {
				continue
			}

			running := tctr.states[string(swarm.TaskStateRunning)]
			complete := tctr.states[string(swarm.TaskStateComplete)]

			var ratio float64
			if entry.val == 0 {
				ratio = 1.0
			} else if running == 0 && complete >= entry.val {
				// All tasks completed successfully (one-shot / cron pattern).
				// Applies to replicated/global services used as jobs,
				// as well as native replicated-job and global-job modes.
				ratio = 1.0
			} else {
				ratio = running / entry.val
			}

			labels := sanitizeMetricLabels(tctr.labels)
			healthRatioGauge.With(labels).Set(ratio)
			emitted[serviceName] = true
		}
	}

	// Services scaled to zero have no tasks and are never visited in the loop above.
	// Emit 1.0 directly from the cache using the stored labels.
	for serviceName, entry := range effectiveDesiredCache {
		if entry.val == 0 && !emitted[serviceName] {
			healthRatioGauge.With(entry.labels).Set(1.0)
		}
	}
}
