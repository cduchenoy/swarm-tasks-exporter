# Swarm tasks exporter

This little Prometheus exporter provides metrics to better monitor Swarm
tasks and their state.

## Summary

* [How it works](#how-it-works)
* [Metrics](#metrics)
* [Install](#install)
* [Configure](#configure)

## How it works?

In its current state, the exporter does:

* Watch swarm events about service create/update/remove, to update the number
  of desired replicas for replicated services.
* Watch swarm events about node create/remove, to update the number of desired
  replicas for global services.
* Regularly poll task list to update the gauge of service tasks segmented
  by state.

## Metrics

| Metric                                     | Type  | Labels                                      | Description                                                                                                                                  |
| ------------------------------------------ | ----- | ------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `swarm_service_desired_replicas`           | Gauge | `stack`, `service`, `service_mode`          | Number of desired replicas as reported by Docker.                                                                                            |
| `swarm_service_effective_desired_replicas` | Gauge | `stack`, `service`, `service_mode`          | Desired replicas after applying the `io.prometheus.desired` label override. Equals `swarm_service_desired_replicas` when no override is set. |
| `swarm_service_replicas_state`             | Gauge | `stack`, `service`, `service_mode`, `state` | Number of tasks per state (running, complete, failed, …).                                                                                    |
| `swarm_service_health_ratio`               | Gauge | `stack`, `service`, `service_mode`          | Ratio of running replicas to effective desired (`1` = healthy, `0` = down, `NaN` = intentionally stopped or one-shot job completed).         |

### io.prometheus.desired label

For global services with placement constraints, Docker reports `desired_replicas = total node count` even though only a subset of nodes matches the constraint. Adding the label `io.prometheus.desired` to the service overrides the value used for `effective_desired_replicas` and `health_ratio`:

```yaml
deploy:
  labels:
    io.prometheus.desired: "3"
```

### health_ratio semantics

| Value       | Meaning                                                                                                    |
| ----------- | ---------------------------------------------------------------------------------------------------------- |
| `1.0`       | All replicas running - or all tasks completed with none running (one-shot / cron pattern) - or scaled to 0 |
| `0 < x < 1` | Degraded - fewer running replicas than expected                                                            |
| `0.0`       | No replicas running and completions below expected count                                                   |

The completion-based rule (`running == 0` and `complete >= effective_desired → 1.0`) applies to all service modes - replicated, global, replicated-job, global-job - making it usable for services that are intentionally run as one-shot jobs regardless of their Docker mode.

## Install

This exporter is available on Docker Hub: [`akerouanton/swarm-tasks-exporter`](https://hub.docker.com/r/akerouanton/swarm-tasks-exporter/):

```sh
docker run -v /var/run/docker.sock:/var/run/docker.sock:ro akerouanton/swarm-tasks-exporter
```

Or, with Docker Swarm:

```yaml
services:
  tasks_exporter:
    image: akerouanton/swarm-tasks-exporter:latest
    command: -log-level error
    volumes:
      - '/var/run/docker.sock:/var/run/docker.sock:ro'
    networks:
      - monit_prometheus
    deploy:
      replicas: 1
      restart_policy:
        condition: on-failure
      placement:
        constraints:
          - node.role == manager
```

The exporter must run on a manager node to access cluster events and the Docker API.

## Configure

You can use the following flags to configure the exporter:

* `-listen-addr <ip:port>`: IP address and port to listen to (default `0.0.0.0:8888`).
* `-poll-delay`: Delay between two task-state polls (default `10s`).
* `-label <name>`: Promote a Docker service label to a Prometheus label on all metrics. Can be repeated.
* `-log-format`: Log format, either `json` or `text` (default `text`).
* `-log-level`: Minimum log level: `debug`, `info`, `warn`, `error`, `fatal` or `panic` (default `info`).

### Custom labels (-label)

The `-label` flag reads a Docker service label and exposes its value as an additional Prometheus label on every metric. This lets you slice metrics by any dimension already present on your services (environment, team, tier, …) without touching PromQL.

Dots in label names are automatically replaced by underscores to comply with Prometheus naming rules (`com.example.env` → `com_example_env`). If a service does not carry the requested label, the Prometheus label is present with an empty value so that all series stay consistent.

**Example** - expose the `environment` and `team` labels:

```yaml
# docker-compose.yml (service definition)
services:
  webapp:
    image: myapp:1.0
    deploy:
      labels:
        environment: "production"
        team: "backend"

  worker:
    image: myworker:1.0
    deploy:
      labels:
        environment: "production"
        team: "data"
```

```yaml
# exporter service
services:
  tasks_exporter:
    image: akerouanton/swarm-tasks-exporter:latest
    command: -label environment -label team
    volumes:
      - '/var/run/docker.sock:/var/run/docker.sock:ro'
    deploy:
      placement:
        constraints:
          - node.role == manager
```

Resulting metrics:

```promql
swarm_service_health_ratio{stack="mystack",service="mystack_webapp",service_mode="replicated",environment="production",team="backend"} 1
swarm_service_health_ratio{stack="mystack",service="mystack_worker",service_mode="replicated",environment="production",team="data"} 1
```

### Environment variables

The exporter honours the standard Docker client environment variables:

* `DOCKER_HOST` - URL of the Docker daemon (default: Unix socket).
* `DOCKER_API_VERSION` - Force a specific Docker API version (e.g. `1.41`).
* `DOCKER_CERT_PATH` - Directory containing TLS certificates.
* `DOCKER_TLS_VERIFY` - Enable TLS verification (`1` to enable).
