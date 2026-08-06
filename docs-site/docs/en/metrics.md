---
title: Metrics and activity
navTitle: Metrics and activity
section: Operate
order: 110
description: Inspect Filegate through REST stats, Prometheus metrics, and activity records.
tags: [metrics, activity, prometheus]
---

# Metrics and activity

This page is for operators who monitor Filegate runtime state, storage pressure, request rates, background loops, and recent operations.

## Observability surfaces

| Surface | Scope | Retention | Use for |
|---|---:|---:|---|
| `GET /v1/stats` | Service snapshot | Request only | Current index, cache, mount, disk, and process state. |
| `/metrics` | Prometheus scrape | Prometheus retention | Request RED metrics, storage gauges, runtime collectors, and background counters. |
| `GET /v1/activity` | Service process | Bounded in-memory ring | Recent operation history for operators and admin UI. |
| Admin System page | Admin app | Current request | Visual summary of stats, metrics, and activity. |

## Enable Prometheus metrics

Put the route settings in the manifest:

```yaml
version: 1
config:
  metrics:
    enabled: true
    path: /metrics
```

Store the secret in the bootstrap config, then apply and restart because the metrics route is static:

```sh
sudo fg config set --config /etc/filegate/conf.yaml --metrics-token '<scrape-token>'
fg config apply -f filegate.manifest.yaml --config /etc/filegate/conf.yaml
sudo systemctl restart filegate
```

If `metrics.token` is set, scrapers must use `Authorization: Bearer <metrics-token>`. If it is empty, Filegate falls back to the REST bearer token. A REST token always exists: Filegate generates and persists one on first start when none is configured. Metrics therefore never becomes open merely because the bootstrap token was left empty.

## Activity query

```sh
curl -fsS -H 'Authorization: Bearer dev-token' \
  'http://127.0.0.1:8080/v1/activity?limit=50&operation=node.mkdir&outcome=succeeded'
```

| Query | Type | Scope | Meaning |
|---|---|---:|---|
| `limit` | integer | Response page | Maximum records returned. |
| `offset` | integer | Response page | Zero-based offset into retained records. |
| `q` | string | Filter | Searches retained activity fields. |
| `operation` | string | Filter | Operation name, for example `node.mkdir`. |
| `outcome` | string | Filter | Outcome value, usually `succeeded` or `failed` for HTTP and S3 requests. |

## Metric reference

See [Metrics reference](/docs/en/reference/metrics) for every Filegate-specific
Prometheus metric and every REST stats field. The [observability
reference](/docs/en/reference/observability) adds PromQL examples, alerting guidance,
and cardinality notes.
