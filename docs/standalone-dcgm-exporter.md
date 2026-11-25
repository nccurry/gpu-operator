# Run DCGM Exporter outside the GPU Operator

The GPU Operator normally manages DCGM Exporter as part of the GPU software
stack. Install the upstream DCGM Exporter Helm chart separately only when you
want that chart release or GitOps application to own the exporter lifecycle.
Keep one exporter workload on each GPU node.

The commands below use the DCGM Exporter 4.10.0 chart and image. Check the
release notes for the supported DCGM version pair. For installation
requirements and current chart repository details, see the [DCGM Exporter
installation guide](https://docs.nvidia.com/datacenter/dcgm/latest/installation/install-dcgm-exporter.html).

## Install with Helm or GitOps

The chart is published in NVIDIA's `gpu-helm-charts` repository. Pin the chart
version in Helm or in your GitOps release configuration:

```bash
helm repo add gpu-helm-charts https://nvidia.github.io/dcgm-exporter/helm-charts
helm repo update
helm search repo gpu-helm-charts/dcgm-exporter --versions
helm show chart gpu-helm-charts/dcgm-exporter --version 4.10.0

helm upgrade --install dcgm-exporter gpu-helm-charts/dcgm-exporter \
  --version 4.10.0 \
  --namespace gpu-monitoring \
  --create-namespace \
  --values dcgm-exporter-values.yaml
```

Keep the chart version and values file together in the GitOps release that owns
this chart. If the cluster does not have the Prometheus Operator CRDs, set
`serviceMonitor.enabled: false`; otherwise the chart creates a `ServiceMonitor`.
Use your cluster's GPU node label and add the tolerations needed by its GPU
nodes. The chart's `tolerations` value replaces its default list.

For ClusterPolicy nodes with the `nvidia.com/gpu:NoSchedule` taint, the
following values make the independent exporter follow the Operator's exporter
deployment label. The DaemonSet releases the node when the driver manager
pauses that label and returns when the label is restored.

```yaml
nodeSelector:
  nvidia.com/gpu.deploy.dcgm-exporter: "true"

tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule

serviceMonitor:
  enabled: true
```

Set `runtimeClassName: nvidia` on clusters that require the NVIDIA runtime
class for GPU device and driver-library access.

## Choose where DCGM runs

The chart runs with an embedded hostengine when `config.sources.dcgm.hostengine`
is omitted. This mode does not need a separate DCGM hostengine Service.

To connect to a DCGM hostengine managed by the GPU Operator, keep the hostengine
enabled and disable only the managed exporter in the active ClusterPolicy or
GPUCluster:

```yaml
spec:
  dcgm:
    enabled: true
  dcgmExporter:
    enabled: false
```

The Operator-managed hostengine Services use port `dcgm` on TCP 5555 and set
`internalTrafficPolicy: Local`. Use the Service for the active API path:

| Active API | DCGM Service endpoint |
| --- | --- |
| ClusterPolicy | `nvidia-dcgm.<operator-namespace>.svc.cluster.local:5555` |
| GPUCluster | `nvidia-dcgm-dra.<operator-namespace>.svc.cluster.local:5555` |

These examples use the default `cluster.local` DNS suffix. Replace it if your
cluster uses a different suffix.

Before connecting to the Operator's hostengine, check that its DCGM version is
the version supported with exporter 4.10.0. Set the endpoint in the independent
chart's values:

```yaml
config:
  sources:
    dcgm:
      hostengine: nvidia-dcgm.gpu-operator.svc.cluster.local:5555
```

For a GPUCluster installation, replace `nvidia-dcgm` with
`nvidia-dcgm-dra`. The `internalTrafficPolicy: Local` setting keeps each
exporter connected to the hostengine on its own node. An external hostengine
uses its own DNS name or address and port in `config.sources.dcgm.hostengine`;
the operator Service policy does not apply to external endpoints.

The chart serves metrics on port `9400` and health checks on port `9401`. With
`hostNetwork: true`, the health listener binds to `127.0.0.1`. Both ports must
be available on every eligible node.

## Switch ownership safely

Update the Operator's source of truth first. For Helm, update the Operator chart
values. For OLM, direct CR management, or GitOps, update the active CR or its
Git-managed manifest. If the independent chart uses the Operator's hostengine,
leave `spec.dcgm.enabled: true` while setting
`spec.dcgmExporter.enabled: false`.

1. Disable the managed exporter in ClusterPolicy or GPUCluster.
2. Wait for the Operator to remove its exporter DaemonSet, Service, and
   monitoring resource. Check that its scrape target has disappeared.
3. Install the upstream Helm chart, or add it to the same GitOps application.
4. Wait for one ready exporter Pod on each intended GPU node. Check that the
   chart's Service and monitoring configuration produce one scrape target per
   Pod and that `/metrics` returns GPU metrics.

Expect a scrape gap between steps 2 and 4. The independent chart creates its
own resources and Service identities; update Prometheus selectors or relabeling
if they depend on the Operator-managed Service or labels.

To roll back, remove the independent Helm release or GitOps application and
wait for its DaemonSet and Service to disappear. Then re-enable
`spec.dcgmExporter.enabled` in the Operator's source of truth and wait for the
managed exporter and its scrape target to return. Do not run both exporters on
the same nodes during the handoff.

## Platform boundaries

This chart is owned by its Helm or GitOps release. The GPU Operator does not
own its DaemonSet or include it in the Operator-managed exporter rollout. The deploy label controls scheduling. Validate the complete driver upgrade
procedure on your platform before relying on the independent exporter during
driver maintenance.

The upstream chart can configure DRA enrichment and the related Kubernetes
read permissions, but its rendered DaemonSet does not include a GPU
`resourceClaim`. DRA enrichment alone does not allocate a DRA GPU claim. The
chart also does not render an OpenShift SecurityContextConstraints resource.
Validate the platform's claim and security policy requirements before using
this independent path on DRA-enabled clusters or OpenShift. Use the
Operator-managed exporter where those integrations must follow the Operator's
existing lifecycle.

Before relying on remote DCGM in production, verify the supported exporter and
DCGM version pair, node-local routing, and exporter readiness when the local
hostengine becomes unavailable. A multi-node cluster check should confirm that
each exporter reports the GPU UUIDs on its own node.

## Render the chart before applying it

Fetch the pinned chart once, then render it locally. Rendering does not contact
the Kubernetes API:

```bash
helm pull gpu-helm-charts/dcgm-exporter \
  --version 4.10.0 \
  --untar \
  --untardir ./charts

helm lint ./charts/dcgm-exporter

# Embedded hostengine, using chart defaults.
helm template dcgm-exporter ./charts/dcgm-exporter \
  --namespace gpu-monitoring \
  --values dcgm-exporter-values.yaml

# Remote hostengine, using the endpoint shown above.
helm template dcgm-exporter ./charts/dcgm-exporter \
  --namespace gpu-monitoring \
  --values dcgm-exporter-values.yaml \
  --set config.sources.dcgm.hostengine=nvidia-dcgm.gpu-operator.svc.cluster.local:5555
```

Review the rendered image, DaemonSet selector, Service port, configuration
ConfigMap, health probes, node selector, and tolerations before applying it.
