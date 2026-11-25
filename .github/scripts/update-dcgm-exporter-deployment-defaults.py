#!/usr/bin/env python3
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Render the pinned exporter chart and update the embedded port/probe data."""

import argparse
import json
import subprocess
import sys
import tempfile
from pathlib import Path

try:
    import yaml
except ImportError as exc:
    raise SystemExit("PyYAML is required. Install it with: pip install pyyaml") from exc


CHART_NAME = "dcgm-exporter"
CHART_VERSION = "4.10.0"
CHART_REPOSITORY = "https://nvidia.github.io/dcgm-exporter/helm-charts"
DEFAULT_OUTPUT = Path("internal/dcgmexporter/deployment_defaults.json")
METRICS_PORT_NAME = "metrics"
HEALTH_PORT_NAME = "health"


def run(command):
    try:
        result = subprocess.run(command, check=True, text=True, capture_output=True)
    except subprocess.CalledProcessError as exc:
        details = exc.stderr.strip()
        raise RuntimeError(f"command failed: {' '.join(command)}\n{details}") from exc
    return result.stdout


def find_one(objects, kind, description):
    matches = [
        item
        for item in objects
        if item
        and item.get("kind") == kind
        and item.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/name")
        == CHART_NAME
    ]
    if len(matches) != 1:
        raise ValueError(f"expected one chart {description}, found {len(matches)}")
    return matches[0]


def named_port(ports, name):
    matches = [port for port in ports if port.get("name") == name]
    if len(matches) != 1:
        raise ValueError(f"expected one container port named {name!r}, found {len(matches)}")
    port = matches[0]
    number = port.get("containerPort")
    if not isinstance(number, int) or not 1 <= number <= 65535:
        raise ValueError(f"container port {name!r} must be a valid TCP port")
    if port.get("hostPort", 0):
        raise ValueError(f"container port {name!r} must not set hostPort")
    if port.get("protocol", "TCP") != "TCP":
        raise ValueError(f"container port {name!r} must use TCP")
    return number


def listen_port(address, description):
    try:
        port = int(address.rsplit(":", 1)[1])
    except (AttributeError, IndexError, ValueError) as exc:
        raise ValueError(f"{description} must have a numeric TCP port") from exc
    if not 1 <= port <= 65535:
        raise ValueError(f"{description} must use a TCP port from 1 to 65535")
    return port


def validate_service(service, metrics_port):
    ports = service.get("spec", {}).get("ports", [])
    matches = [port for port in ports if port.get("name") == METRICS_PORT_NAME]
    if len(matches) != 1:
        raise ValueError("expected one chart Service port named metrics")
    port = matches[0]
    if port.get("port") != metrics_port:
        raise ValueError("chart Service and container metrics ports do not match")
    target_port = port.get("targetPort", port.get("port"))
    if target_port not in (metrics_port, METRICS_PORT_NAME):
        raise ValueError("chart Service targetPort does not match the container metrics port")


def validate_probe(probe, name):
    handler = probe.get("httpGet") if isinstance(probe, dict) else None
    if not handler or handler.get("port") != HEALTH_PORT_NAME or not handler.get("path"):
        raise ValueError(f"chart {name} probe must use HTTP GET on the named health port")


def project(manifests, chart_version):
    objects = list(yaml.safe_load_all(manifests))
    daemonset = find_one(objects, "DaemonSet", "DaemonSet")
    service = find_one(objects, "Service", "metrics Service")
    config_maps = [
        item
        for item in objects
        if item
        and item.get("kind") == "ConfigMap"
        and item.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/name")
        == CHART_NAME
        and "config.yaml" in item.get("data", {})
    ]
    if len(config_maps) != 1:
        raise ValueError(f"expected one chart config ConfigMap with config.yaml, found {len(config_maps)}")
    config_map = config_maps[0]

    containers = daemonset.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
    matches = []
    for container in containers:
        ports = container.get("ports", [])
        if any(port.get("name") == METRICS_PORT_NAME for port in ports) and any(
            port.get("name") == HEALTH_PORT_NAME for port in ports
        ):
            matches.append(container)
    if len(matches) != 1:
        raise ValueError(f"expected one exporter container with metrics and health ports, found {len(matches)}")

    container = matches[0]
    ports = container.get("ports", [])
    metrics_port = named_port(ports, METRICS_PORT_NAME)
    health_port = named_port(ports, HEALTH_PORT_NAME)
    if metrics_port == health_port:
        raise ValueError("metrics and health ports must be distinct")

    config = yaml.safe_load(config_map["data"]["config.yaml"])
    metrics_address = config.get("outputs", {}).get("prometheus", {}).get("listen", "")
    health_address = config.get("server", {}).get("listen_health", "")
    if listen_port(metrics_address, "config.outputs.prometheus.listen") != metrics_port:
        raise ValueError("chart config and container metrics ports do not match")
    if listen_port(health_address, "config.server.listen_health") != health_port:
        raise ValueError("chart config and container health ports do not match")
    validate_service(service, metrics_port)

    for name in ("startupProbe", "livenessProbe", "readinessProbe"):
        validate_probe(container.get(name), name)

    normalized_ports = []
    for port in ports:
        normalized = dict(port)
        normalized.setdefault("protocol", "TCP")
        normalized_ports.append(normalized)

    return {
        "_generatedBy": "make update-dcgm-exporter-deployment-defaults",
        "chartVersion": chart_version,
        "ports": normalized_ports,
        "startupProbe": container["startupProbe"],
        "livenessProbe": container["livenessProbe"],
        "readinessProbe": container["readinessProbe"],
    }


def get_chart(helm, chart_path, version, temporary_directory):
    if chart_path:
        chart = Path(chart_path)
    else:
        run(
            [
                helm,
                "pull",
                "--repo",
                CHART_REPOSITORY,
                CHART_NAME,
                "--version",
                version,
                "--destination",
                str(temporary_directory),
            ]
        )
        chart = temporary_directory / f"{CHART_NAME}-{version}.tgz"

    metadata = yaml.safe_load(run([helm, "show", "chart", str(chart)]))
    if metadata.get("name") != CHART_NAME:
        raise ValueError(f"chart name is {metadata.get('name')!r}, expected {CHART_NAME!r}")
    if metadata.get("version") != version:
        raise ValueError(f"chart version is {metadata.get('version')!r}, expected pinned version {version!r}")
    return chart


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chart", help="local chart directory or archive; defaults to the pinned public chart")
    parser.add_argument("--chart-version", default=CHART_VERSION, help="required DCGM Exporter chart version")
    parser.add_argument("--helm", default="helm", help="Helm executable")
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT, help="generated defaults JSON path")
    args = parser.parse_args()

    try:
        with tempfile.TemporaryDirectory(prefix="dcgm-exporter-chart-") as temporary_directory:
            chart = get_chart(args.helm, args.chart, args.chart_version, Path(temporary_directory))
            rendered = run(
                [
                    args.helm,
                    "template",
                    "gpu-operator",
                    str(chart),
                    "--namespace",
                    "gpu-operator",
                    "--kube-version",
                    "1.31.0",
                ]
            )
            defaults = project(rendered, args.chart_version)

        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(defaults, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    except (AttributeError, KeyError, OSError, RuntimeError, TypeError, ValueError) as exc:
        print(exc, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
