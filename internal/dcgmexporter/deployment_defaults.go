/**
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
**/

package dcgmexporter

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// MetricsPortName is the named port exposed by the exporter container.
	MetricsPortName = "metrics"
)

// deploymentDefaultsJSON is generated from the pinned DCGM Exporter Helm chart.
//
//go:embed deployment_defaults.json
var deploymentDefaultsJSON []byte

// DeploymentDefaults contains the chart-owned container ports and HTTP probes
// that both Operator deployment paths share.
type DeploymentDefaults struct {
	ChartVersion   string                 `json:"chartVersion"`
	Ports          []corev1.ContainerPort `json:"ports"`
	StartupProbe   *corev1.Probe          `json:"startupProbe"`
	LivenessProbe  *corev1.Probe          `json:"livenessProbe"`
	ReadinessProbe *corev1.Probe          `json:"readinessProbe"`
}

var (
	defaultsOnce sync.Once
	defaults     DeploymentDefaults
	defaultsErr  error
)

// DefaultDeploymentDefaults returns the pinned chart defaults as a copy.
func DefaultDeploymentDefaults() (DeploymentDefaults, error) {
	defaultsOnce.Do(func() {
		defaultsErr = json.Unmarshal(deploymentDefaultsJSON, &defaults)
	})
	if defaultsErr != nil {
		return DeploymentDefaults{}, fmt.Errorf("read dcgm-exporter deployment defaults: %w", defaultsErr)
	}

	copy := defaults
	copy.Ports = append([]corev1.ContainerPort(nil), defaults.Ports...)
	copy.StartupProbe = defaults.StartupProbe.DeepCopy()
	copy.LivenessProbe = defaults.LivenessProbe.DeepCopy()
	copy.ReadinessProbe = defaults.ReadinessProbe.DeepCopy()
	return copy, nil
}

// Port returns the number of a named chart container port.
func (defaults DeploymentDefaults) Port(name string) (int32, error) {
	var port int32
	count := 0
	for _, candidate := range defaults.Ports {
		if candidate.Name == name {
			port = candidate.ContainerPort
			count++
		}
	}
	if count != 1 {
		return 0, fmt.Errorf("expected one container port named %q, got %d", name, count)
	}
	return port, nil
}

// DefaultMetricsPort returns the metrics port from the pinned chart.
func DefaultMetricsPort() (int32, error) {
	defaults, err := DefaultDeploymentDefaults()
	if err != nil {
		return 0, err
	}
	return defaults.Port(MetricsPortName)
}

// DefaultHealthPort returns the health port from the pinned chart.
func DefaultHealthPort() (int32, error) {
	defaults, err := DefaultDeploymentDefaults()
	if err != nil {
		return 0, err
	}
	return defaults.Port(HealthPortName)
}

// ApplyDeploymentDefaults overlays chart ports and probes on one exporter
// container while preserving additional container ports.
func ApplyDeploymentDefaults(container *corev1.Container, hostNetwork bool) error {
	if container == nil {
		return fmt.Errorf("apply dcgm-exporter deployment defaults: container is nil")
	}
	defaults, err := DefaultDeploymentDefaults()
	if err != nil {
		return err
	}
	if defaults.StartupProbe == nil || defaults.LivenessProbe == nil || defaults.ReadinessProbe == nil {
		return fmt.Errorf("pinned chart does not provide all exporter HTTP probes")
	}
	for _, probe := range []*corev1.Probe{defaults.StartupProbe, defaults.LivenessProbe, defaults.ReadinessProbe} {
		if probe.HTTPGet == nil {
			return fmt.Errorf("pinned chart exporter probe does not use HTTP GET")
		}
	}

	container.Ports = mergeContainerPorts(container.Ports, defaults.Ports)
	container.StartupProbe = probeForNetwork(defaults.StartupProbe, hostNetwork)
	container.LivenessProbe = probeForNetwork(defaults.LivenessProbe, hostNetwork)
	container.ReadinessProbe = probeForNetwork(defaults.ReadinessProbe, hostNetwork)
	return nil
}

func mergeContainerPorts(existing, chartPorts []corev1.ContainerPort) []corev1.ContainerPort {
	chartNames := make(map[string]struct{}, len(chartPorts))
	for _, port := range chartPorts {
		chartNames[port.Name] = struct{}{}
	}

	ports := append([]corev1.ContainerPort(nil), chartPorts...)
	for _, port := range existing {
		if _, exists := chartNames[port.Name]; !exists {
			ports = append(ports, port)
		}
	}
	return ports
}

func probeForNetwork(probe *corev1.Probe, hostNetwork bool) *corev1.Probe {
	probe.HTTPGet.Host = ""
	if hostNetwork {
		probe.HTTPGet.Host = "127.0.0.1"
	}
	probe.HTTPGet.Port = intstr.FromString(HealthPortName)
	return probe
}
