# kubehoard

**Find namespaces hoarding CPU and memory — then fix them.**

kubehoard is a CLI for Kubernetes and OpenShift that compares resource
*requests* against *actual usage* from the metrics API, ranks namespaces by
their overprovisioning factor, and flags pods running dangerously close to
their *limits* (OOMKill/throttling risk). When you've reviewed the findings,
`suggest` turns them into an editable patch plan and `apply` right-sizes the
owner workloads — the rolling restart happens automatically.

No operator, no CRDs, no agent: a single static binary that talks to the
Kube API and `metrics.k8s.io`, suitable for one-off scans, cron jobs and CI.

## What it measures

| Metric | Formula | Meaning |
|---|---|---|
| Overprovisioning factor | request ÷ actual usage | ≥ 10x = warning, ≥ 50x = critical (configurable) |
| Limit utilization | usage ÷ limit × 100 % | ≥ 90 % = limit risk (configurable) |

The two problems are deliberately kept apart: hoarding wastes cluster
capacity, limit pressure threatens workload stability. The report marks
them with separate badges/columns.

## Install

```sh
go install github.com/shushyu/kubehoard/cmd/kubehoard@latest
```

Or build from source:

```sh
git clone https://github.com/shushyu/kubehoard
cd kubehoard
make build   # produces ./kubehoard
```

Requires a cluster with the metrics API available (`metrics-server`, or the
Prometheus adapter on OpenShift). Kubeconfig resolution works like kubectl:
`--kubeconfig` → `$KUBECONFIG` → in-cluster service account → `~/.kube/config`.

## Quick start

```sh
# Overview of the whole cluster, worst hoarders first (default format: table).
# Also prints a container-level breakdown of hoarding pods.
kubehoard

# Scan specific namespaces, or exclude system namespaces
kubehoard -n team-a,team-b
kubehoard --exclude-regex '^(kube-|openshift-)'

# Filter namespaces by label (server-side)
kubehoard --label-selector team=platform

# Self-contained HTML report / JSON for automation
kubehoard --format=html --output report.html
kubehoard --format=json | jq '.namespaces[:5] | .[] | {name, sortFactor}'

# Container detail for all pods, or none
kubehoard --detail=all
kubehoard --detail=none

# Custom thresholds
kubehoard --warn-factor 5 --crit-factor 20 --limit-warn 85
```

Example terminal output:

```
kubehoard – 2026-07-10 09:22:11
Namespaces: 2 | Pods: 3 | Hoarders: 0 warning / 2 critical | Limit risk: 1
CPU total: requests 9.00 cores, usage 120m
Mem total: requests 18.00 GiB, usage 1.38 GiB

NAMESPACE     PODS  CPU REQ     CPU USE  CPU FACTOR  MEM REQ    MEM USE    MEM FACTOR  STATUS
opendesk-dev  2     8.00 cores  120m     66.7x       16.00 GiB  900.0 MiB  18.2x       CRITICAL +LIMIT
monitoring    1     1.00 cores  0m       ≥1000x      2.00 GiB   512.0 MiB  4.0x        CRITICAL
```

## Fixing hoarders: suggest → apply

```sh
# 1. Generate a plan: every container with factor >= warn-factor gets a
#    request suggestion derived from the highest measured usage across all
#    replicas, multiplied by --headroom and rounded up to sane steps.
#    Limits are NEVER suggested automatically.
kubehoard suggest -n team-a --headroom 2.0 --output plan.yaml

# 2. Review and edit the plan (change values, delete targets, add
#    cpuLimit/memLimit by hand). Empty fields mean: leave unchanged.
$EDITOR plan.yaml

# 3. Apply: a server-side dry-run of ALL targets first (validates existence,
#    RBAC, LimitRanges, quotas, admission webhooks), then a confirmation
#    prompt, then a strategic merge patch on each workload's pod template.
#    The patch triggers the rolling restart automatically.
kubehoard apply plan.yaml            # interactive
kubehoard apply --dry-run plan.yaml  # validate only
kubehoard apply --yes plan.yaml      # non-interactive (automation)
```

To base the plan on exactly the numbers you reviewed (instead of a fresh
snapshot), go through a JSON report:

```sh
kubehoard scan --resolve-owners --format=json --output report.json
kubehoard suggest --from report.json --output plan.yaml
```

A plan looks like this — plain YAML, made to be edited:

```yaml
generatedAt: "2026-07-10T09:30:00Z"
headroom: 2
minFactor: 10
targets:
  - namespace: team-a
    kind: Deployment
    workload: web
    container: app
    cpuRequest: 125m        # suggestion; edit or delete freely
    memRequest: 320Mi
    observed:               # informational, ignored by apply
      cpuRequest: 2.00 cores
      memRequest: 4.00 GiB
      cpuUsage: 60m
      memUsage: 150.0 MiB
      pods: 2
skipped:
  - team-a/batch (Job not supported by apply) container=runner
```

### How apply works

- **Owner resolution:** pod → ReplicaSet → **Deployment**, or
  **StatefulSet** / **DaemonSet** directly. Other owners (Jobs, CronJobs,
  bare pods, ReplicaSets without a Deployment, DeploymentConfigs) end up
  under `skipped` in the plan and must be handled manually.
- **Nothing changes if anything fails validation:** every target is
  dry-run against the API server before the first real patch.
- **Minimal patches:** strategic merge with the container name as merge
  key — only the named container and only the specified resource fields
  are touched.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | clean (or findings, but `--fail-on=none`, the default) |
| 1 | technical error (API unreachable, patch failed, ...) |
| 2 | flag/usage error |
| 3 | findings above the `--fail-on` threshold (`warning` or `critical`) |

```sh
# cron/CI: alert on critical hoarders, distinguish from broken scans
kubehoard --fail-on=critical --format=json --output report.json
```

## RBAC

- `scan`: cluster-wide `get`/`list` on `namespaces`, `pods` and
  `pods.metrics.k8s.io` (read-only; sufficient for cron).
- `suggest`: additionally `get` on `replicasets.apps` (owner resolution).
- `apply`: additionally `patch` on `deployments.apps`, `statefulsets.apps`,
  `daemonsets.apps`. These are write permissions — keep them off your
  read-only cron service account; `apply` is meant for interactive admin
  use.

## Good to know / limitations

- **Point-in-time metrics, not p95.** Suggestions are based on a single
  metrics API snapshot. For spiky workloads, scan during representative
  load, pick a generous `--headroom`, or adjust the plan upwards.
  (Prometheus-based percentiles are the natural next step and on the
  roadmap.)
- **GitOps-managed workloads** (Argo CD, Flux, Helm): a direct patch will
  be reverted on the next sync. Use the plan as the source for the change
  in Git instead of applying it directly.
- Only **running** pods are evaluated (Succeeded/Failed/Pending would skew
  requests or lack metrics). Pods without metrics (recently started) count
  into the sums but are excluded from factor computation.
- **Init containers** are listed separately (per-resource maximum, matching
  scheduler semantics) and excluded from the factors — they consume nothing
  at runtime.
- Near-zero usage never produces an infinite factor; it is clamped to
  `--cap-factor` (default 1000) and rendered as `≥1000x`.
- In JSON output, factors are `null` when not computable (no request/limit
  set), otherwise `{"value": 12.3}` or `{"value": 1000, "capped": true}`.

## Project layout

```
cmd/kubehoard/       entry point, subcommands scan | suggest | apply
internal/model/      data structures + pure calculation logic (unit-tested)
internal/collector/  Kube API + metrics API, owner resolution, aggregation
internal/report/     terminal, JSON and HTML rendering
internal/plan/       suggestion logic, plan format, patch construction (unit-tested)
```

## Development

```sh
make build   # go build
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt check
```

Deliberately out of scope: operators, CRDs, controllers, servers, history
over time. kubehoard is a CLI — the HTML report is static, changes go
through the suggest/apply workflow.

## License

MIT — see [LICENSE](LICENSE).
