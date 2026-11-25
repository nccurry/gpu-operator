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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestDefaultDeploymentDefaults(t *testing.T) {
	defaults, err := DefaultDeploymentDefaults()
	require.NoError(t, err)
	assert.NotEmpty(t, defaults.ChartVersion)

	metricsPort, err := defaults.Port(MetricsPortName)
	require.NoError(t, err)
	healthPort, err := defaults.Port(HealthPortName)
	require.NoError(t, err)
	assert.Positive(t, metricsPort)
	assert.Positive(t, healthPort)
	assert.NotEqual(t, metricsPort, healthPort)

	for _, probe := range []*corev1.Probe{defaults.StartupProbe, defaults.LivenessProbe, defaults.ReadinessProbe} {
		require.NotNil(t, probe)
		require.NotNil(t, probe.HTTPGet)
		assert.Equal(t, intstr.FromString(HealthPortName), probe.HTTPGet.Port)
	}
}

func TestApplyDeploymentDefaults(t *testing.T) {
	defaults, err := DefaultDeploymentDefaults()
	require.NoError(t, err)
	container := corev1.Container{
		Name: "operator-container-name",
		Ports: []corev1.ContainerPort{
			{Name: MetricsPortName, ContainerPort: 1234},
			{Name: "admin", ContainerPort: 9100},
		},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
	}

	require.NoError(t, ApplyDeploymentDefaults(&container, false))
	assert.Equal(t, "operator-container-name", container.Name)
	require.Len(t, container.Ports, len(defaults.Ports)+1)
	metricsPort, err := defaults.Port(MetricsPortName)
	require.NoError(t, err)
	assert.Equal(t, metricsPort, container.Ports[0].ContainerPort)
	assert.Equal(t, corev1.ContainerPort{Name: "admin", ContainerPort: 9100}, container.Ports[2])
	cpuLimit := container.Resources.Limits[corev1.ResourceCPU]
	assert.Equal(t, "1", cpuLimit.String())
	assert.Equal(t, defaults.StartupProbe, container.StartupProbe)
	assert.Equal(t, defaults.LivenessProbe, container.LivenessProbe)
	assert.Equal(t, defaults.ReadinessProbe, container.ReadinessProbe)
	assert.Empty(t, container.StartupProbe.HTTPGet.Host)

	require.NoError(t, ApplyDeploymentDefaults(&container, true))
	assert.Equal(t, "127.0.0.1", container.StartupProbe.HTTPGet.Host)
	assert.Equal(t, "127.0.0.1", container.LivenessProbe.HTTPGet.Host)
	assert.Equal(t, "127.0.0.1", container.ReadinessProbe.HTTPGet.Host)

	require.NoError(t, ApplyDeploymentDefaults(&container, false))
	assert.Empty(t, container.StartupProbe.HTTPGet.Host)
}

func TestApplyDeploymentDefaultsRejectsNilContainer(t *testing.T) {
	require.Error(t, ApplyDeploymentDefaults(nil, false))
}
