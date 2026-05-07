package main

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

type effectiveCacheEntry struct {
	val    float64
	labels prometheus.Labels // sanitized, ready for gauge
}

var (
	desiredReplicasGauge  *prometheus.GaugeVec
	effectiveDesiredGauge *prometheus.GaugeVec

	effectiveMu           sync.RWMutex
	effectiveDesiredCache = make(map[string]effectiveCacheEntry)

	nodeCount atomic.Int64
)

func configureDesiredReplicasGauge() {
	labels := append([]string{
		"stack",
		"service",
		"service_mode",
	}, customLabels...)

	desiredReplicasGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarm_service_desired_replicas",
		Help: "Number of desired replicas for swarm services",
	}, sanitizeLabelNames(labels))
	prometheus.MustRegister(desiredReplicasGauge)
}

func configureEffectiveDesiredGauge() {
	labels := append([]string{
		"stack",
		"service",
		"service_mode",
	}, customLabels...)

	effectiveDesiredGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarm_service_effective_desired_replicas",
		Help: "Effective desired replicas after io.prometheus.desired label override",
	}, sanitizeLabelNames(labels))
	prometheus.MustRegister(effectiveDesiredGauge)
}

func initDesiredReplicasGauge(ctx context.Context, cli *client.Client) error {
	services, err := cli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return err
	}

	nodes, err := cli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return err
	}
	nodeCount.Store(int64(len(nodes)))

	metaMu.Lock()
	for _, svc := range services {
		metadataCache[svc.ID] = buildMetadata(svc)
	}
	metaMu.Unlock()

	for _, svc := range services {
		metaMu.RLock()
		md := metadataCache[svc.ID]
		metaMu.RUnlock()
		updateServiceReplicasGauge(svc, md)
	}

	return nil
}

func updateServiceReplicasGauge(svc swarm.Service, metadata serviceMetadata) {
	// io.prometheus.desired label overrides the Docker-reported count.
	// Useful for global services with placement constraints where Docker
	// reports all nodes but only a subset matches the constraint.
	if override, ok := svc.Spec.Annotations.Labels["io.prometheus.desired"]; ok {
		if val, err := strconv.ParseFloat(override, 64); err == nil && val >= 0 {
			setDesiredReplicasGauge(metadata, val)
			setEffective(metadata, val)
			return
		}
	}

	var effective float64
	switch {
	case svc.Spec.Mode.Replicated != nil:
		effective = float64(*svc.Spec.Mode.Replicated.Replicas)
	case svc.Spec.Mode.ReplicatedJob != nil && svc.Spec.Mode.ReplicatedJob.TotalCompletions != nil:
		effective = float64(*svc.Spec.Mode.ReplicatedJob.TotalCompletions)
	default: // global and global-job: one task per node
		effective = float64(nodeCount.Load())
	}
	setDesiredReplicasGauge(metadata, effective)
	setEffective(metadata, effective)
}

func setEffective(metadata serviceMetadata, val float64) {
	labels := prometheus.Labels{
		"stack":        metadata.stack,
		"service":      metadata.service,
		"service_mode": metadata.serviceMode,
	}
	for k, v := range metadata.customLabels {
		labels[k] = v
	}
	effectiveMu.Lock()
	effectiveDesiredCache[metadata.service] = effectiveCacheEntry{
		val:    val,
		labels: sanitizeMetricLabels(labels),
	}
	effectiveMu.Unlock()
	setEffectiveDesiredGauge(metadata, val)
}

func setDesiredReplicasGauge(metadata serviceMetadata, val float64) {
	labels := prometheus.Labels{
		"stack":        metadata.stack,
		"service":      metadata.service,
		"service_mode": metadata.serviceMode,
	}
	for k, v := range metadata.customLabels {
		labels[k] = v
	}
	desiredReplicasGauge.With(sanitizeMetricLabels(labels)).Set(val)
}

func setEffectiveDesiredGauge(metadata serviceMetadata, val float64) {
	labels := prometheus.Labels{
		"stack":        metadata.stack,
		"service":      metadata.service,
		"service_mode": metadata.serviceMode,
	}
	for k, v := range metadata.customLabels {
		labels[k] = v
	}
	effectiveDesiredGauge.With(sanitizeMetricLabels(labels)).Set(val)
}

func listenSwarmEvents(ctx context.Context, cli *client.Client) error {
	filterArgs := filters.NewArgs()
	filterArgs.Add("type", "service")
	filterArgs.Add("type", "node")

	evtCh, errCh := cli.Events(ctx, events.ListOptions{
		Since:   time.Now().Format(time.RFC3339),
		Filters: filterArgs,
	})

	logrus.Info("Start listening for new Swarm events...")

	for {
		select {
		case err := <-errCh:
			// @TODO: auto-reconnect when connection lost
			return err
		case evt := <-evtCh:
			go func(evt events.Message) {
				logrus.WithFields(logrus.Fields{
					"type":       evt.Type,
					"action":     evt.Action,
					"actor.id":   evt.Actor.ID,
					"actor.name": evt.Actor.Attributes["name"],
				}).Info("New event received.")

				if err := processEvent(ctx, cli, evt); err != nil {
					logrus.Error(err)
				}
			}(evt)
		}
	}

	return nil
}

func processEvent(ctx context.Context, cli *client.Client, evt events.Message) error {
	if evt.Type == "node" {
		// Re-init desired replicas gauge when a node is added/deleted,
		// to be sure global services have the right number of desired replicas
		if evt.Action == "create" {
			nodeCount.Add(1)
			initDesiredReplicasGauge(ctx, cli)
		} else if evt.Action == "remove" {
			nodeCount.Add(-1)
			initDesiredReplicasGauge(ctx, cli)
		}
		return nil
	}

	sid := evt.Actor.ID

	if evt.Action == "remove" {
		metaMu.RLock()
		metadata, ok := metadataCache[sid]
		metaMu.RUnlock()
		if !ok {
			return errors.New("no cached metadata found for removed service")
		}

		setDesiredReplicasGauge(metadata, 0)

		metaMu.Lock()
		delete(metadataCache, sid)
		metaMu.Unlock()

		effectiveMu.Lock()
		delete(effectiveDesiredCache, metadata.service)
		effectiveMu.Unlock()

		return nil
	}

	svc, _, err := cli.ServiceInspectWithRaw(ctx, sid, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	md := buildMetadata(svc)
	metaMu.Lock()
	metadataCache[sid] = md
	metaMu.Unlock()

	updateServiceReplicasGauge(svc, md)

	return nil
}
