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
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const validNativeConfig = `version: 2
metrics:
  - name: default
    file: /etc/dcgm-exporter/default-counters.csv
    every: 30s
    enrichment:
      kubernetes: auto
      container: false
      dra: false
      hpc_jobs: false
outputs:
  prometheus:
    type: prometheus
    schema: dcgm/v1
    listen: :9400
server:
  listen_health: :9401
`

func TestValidateNativeConfigSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector *corev1.ConfigMapKeySelector
		wantErr  string
	}{
		{name: "nil selector"},
		{name: "valid selector", selector: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "config-v1"}, Key: "exporter.yaml"}},
		{name: "missing name", selector: &corev1.ConfigMapKeySelector{Key: "exporter.yaml"}, wantErr: "name is required"},
		{name: "missing key", selector: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "config-v1"}}, wantErr: "key is required"},
		{name: "optional", selector: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "config-v1"}, Key: "exporter.yaml", Optional: new(true)}, wantErr: "optional must be false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateNativeConfigSelector(test.selector)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestValidateWebConfigSelector(t *testing.T) {
	selector := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "web-config-v1"},
		Key:                  "web.yaml",
	}
	require.NoError(t, ValidateWebConfigSelector(selector))
	selector.Optional = new(true)
	require.ErrorContains(t, ValidateWebConfigSelector(selector), "optional must be false")
	selector.Optional = nil
	selector.Key = ".."
	require.ErrorContains(t, ValidateWebConfigSelector(selector), "single Secret data key")
}

func TestResolveNativeConfig(t *testing.T) {
	selector := &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "exporter-config-v1"},
		Key:                  "exporter.yaml",
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: selector.Name, Namespace: "operator"},
		Immutable:  ptr.To(true),
		Data:       map[string]string{selector.Key: validNativeConfig},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()

	resolved, err := ResolveNativeConfig(context.Background(), reader, "operator", selector, NativeConfigOptions{})
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, selector.Name, resolved.Name)
	assert.Equal(t, selector.Key, resolved.Key)

	otherNamespace := "other"
	_, err = ResolveNativeConfig(context.Background(), reader, otherNamespace, selector, NativeConfigOptions{})
	require.ErrorContains(t, err, "not found in the Operator namespace")
}

func TestResolveNativeConfigRejectsInvalidReferences(t *testing.T) {
	validConfigMap := func(immutable *bool, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "operator"},
			Immutable:  immutable,
			Data:       data,
		}
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	for _, test := range []struct {
		name      string
		configMap *corev1.ConfigMap
		wantErr   string
	}{
		{name: "mutable ConfigMap", configMap: validConfigMap(nil, map[string]string{"exporter.yaml": validNativeConfig}), wantErr: "must be immutable"},
		{name: "missing key", configMap: validConfigMap(ptr.To(true), map[string]string{}), wantErr: "key \"exporter.yaml\" was not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.configMap).Build()
			_, err := ResolveNativeConfig(context.Background(), reader, "operator", &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "config"},
				Key:                  "exporter.yaml",
			}, NativeConfigOptions{})
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestResolveWebConfig(t *testing.T) {
	selector := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "web-config-v1"},
		Key:                  "web.yaml",
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: selector.Name, Namespace: "operator"},
		Data:       map[string][]byte{selector.Key: []byte("private-web-config")},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	resolved, err := ResolveWebConfig(context.Background(), reader, "operator", selector)
	require.NoError(t, err)
	assert.Equal(t, &WebConfigReference{Name: selector.Name}, resolved)

	selector.Key = "missing.yaml"
	_, err = ResolveWebConfig(context.Background(), reader, "operator", selector)
	require.ErrorContains(t, err, "key \"missing.yaml\" was not found")
	assert.NotContains(t, err.Error(), "private-web-config")
	selector.Key = "empty.yaml"
	secret.Data[selector.Key] = []byte{}
	require.NoError(t, reader.Update(context.Background(), secret))
	_, err = ResolveWebConfig(context.Background(), reader, "operator", selector)
	require.ErrorContains(t, err, "key \"empty.yaml\" is empty")
}

func TestValidateNativeConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		options NativeConfigOptions
		wantErr string
	}{
		{name: "valid defaults", config: validNativeConfig},
		{
			name:    "invalid YAML",
			config:  "version: [",
			wantErr: "must contain a YAML mapping",
		},
		{
			name:    "wrong version",
			config:  "version: 1\n" + validNativeConfig[strings.Index(validNativeConfig, "metrics:"):],
			wantErr: "version must be 2",
		},
		{
			name:    "metrics listener conflicts with Service",
			config:  replaceOnce(validNativeConfig, "listen: :9400", "listen: 127.0.0.1:9400"),
			wantErr: "must define a Prometheus output that uses a wildcard address on port 9400",
		},
		{
			name:    "metrics port conflicts with Service",
			config:  replaceOnce(validNativeConfig, "listen: :9400", "listen: :9500"),
			wantErr: "must define a Prometheus output that uses a wildcard address on port 9400",
		},
		{
			name:   "named Prometheus output on Service port",
			config: replaceOnce(validNativeConfig, "  prometheus:\n", "  team_metrics:\n"),
		},
		{
			name:   "single named Prometheus output uses the default Service port",
			config: replaceOnce(replaceOnce(validNativeConfig, "  prometheus:\n", "  team_metrics:\n"), "    listen: :9400\n", ""),
		},
		{
			name:    "health port conflicts with probes",
			config:  replaceOnce(validNativeConfig, "listen_health: :9401", "listen_health: :9501"),
			wantErr: "must bind the Operator health port 9401",
		},
		{
			name:    "host network health listener must be loopback",
			config:  replaceOnce(validNativeConfig, "listen_health: :9401", "listen_health: 0.0.0.0:9401"),
			options: NativeConfigOptions{HostNetwork: true},
			wantErr: "must bind the Operator health port 9401",
		},
		{
			name:    "legacy CSV path requires migration",
			config:  replaceOnce(validNativeConfig, DefaultMetricsFilePath, oldMetricsFilePath),
			wantErr: "former custom metrics path",
		},
		{
			name:    "custom CSV path requires selected ConfigMap",
			config:  replaceOnce(validNativeConfig, DefaultMetricsFilePath, CustomMetricsFilePath),
			wantErr: "without dcgmExporter.config.name",
		},
		{
			name:   "selected custom CSV must be referenced",
			config: validNativeConfig,
			options: NativeConfigOptions{
				MetricsConfigName: "metrics-v1",
			},
			wantErr: "must reference the mounted custom metrics file",
		},
		{
			name:   "custom CSV path with selected ConfigMap",
			config: replaceOnce(validNativeConfig, DefaultMetricsFilePath, CustomMetricsFilePath),
			options: NativeConfigOptions{
				MetricsConfigName: "metrics-v1",
			},
		},
		{
			name:    "web config requires selected Secret key path",
			config:  replaceOnce(validNativeConfig, "  listen_health: :9401", "  listen_health: :9401\n  web_config_file: /etc/dcgm-exporter/operator/web-config/web.yaml"),
			wantErr: "requires dcgmExporter.webConfig",
		},
		{
			name:    "web config must use selected Secret key path",
			config:  replaceOnce(validNativeConfig, "  listen_health: :9401", "  listen_health: :9401\n  web_config_file: /etc/dcgm-exporter/operator/web-config/wrong.yaml"),
			options: NativeConfigOptions{WebConfigFilePath: WebConfigFilePathForKey("web.yaml")},
			wantErr: "must select the projected webConfig key",
		},
		{
			name:    "web config path matches Secret key",
			config:  replaceOnce(validNativeConfig, "  listen_health: :9401", "  listen_health: :9401\n  web_config_file: /etc/dcgm-exporter/operator/web-config/web.yaml"),
			options: NativeConfigOptions{WebConfigFilePath: WebConfigFilePathForKey("web.yaml")},
		},
		{
			name:    "DRA source unavailable in ClusterPolicy",
			config:  replaceOnce(validNativeConfig, "      dra: false", "      dra: auto"),
			wantErr: "DRA enrichment without an available DRA source",
		},
		{
			name:    "DRA source unavailable in ClusterPolicy",
			config:  replaceOnce(validNativeConfig, "server:\n", "sources:\n  dra:\n    mode: kubernetes\nserver:\n"),
			wantErr: "requires the GPUCluster DRA exporter",
		},
		{
			name:    "DRA source with supported DRA workload",
			config:  replaceOnce(replaceOnce(validNativeConfig, "      dra: false", "      dra: auto"), "server:\n", "sources:\n  dra:\n    mode: kubernetes\nserver:\n"),
			options: NativeConfigOptions{DRA: true},
		},
		{
			name:    "HPC source requires matching mount",
			config:  replaceOnce(validNativeConfig, "server:\n", "sources:\n  hpc_jobs:\n    mapping_dir: /var/lib/jobs\nserver:\n"),
			wantErr: "must match the mounted HPC job mapping directory",
		},
		{
			name:    "HPC source with matching mount",
			config:  replaceOnce(validNativeConfig, "server:\n", "sources:\n  hpc_jobs:\n    mapping_dir: /var/lib/jobs\nserver:\n"),
			options: NativeConfigOptions{HPCJobMappingDir: "/var/lib/jobs"},
		},
		{
			name:    "HPC enrichment requires source",
			config:  replaceOnce(validNativeConfig, "      hpc_jobs: false", "      hpc_jobs: auto"),
			wantErr: "HPC job enrichment without a mounted HPC job mapping source",
		},
		{
			name:   "Kubernetes source requests API access",
			config: replaceOnce(validNativeConfig, "server:\n", "sources:\n  kubernetes:\n    pod_uid: true\nserver:\n"),
		},
		{
			name: "loopback hostengine requires explicit host network opt-in",
			config: replaceOnce(
				replaceOnce(validNativeConfig, "server:\n", "sources:\n  dcgm:\n    hostengine: localhost:5555\nserver:\n"),
				"listen_health: :9401",
				"listen_health: 127.0.0.1:9401",
			),
			wantErr: "set dcgmExporter.hostNetwork=true",
		},
		{
			name: "loopback hostengine works with explicit host network opt-in",
			config: replaceOnce(
				replaceOnce(validNativeConfig, "server:\n", "sources:\n  dcgm:\n    hostengine: localhost:5555\nserver:\n"),
				"listen_health: :9401",
				"listen_health: 127.0.0.1:9401",
			),
			options: NativeConfigOptions{HostNetwork: true},
		},
		{
			name:    "native listener rejects Kubernetes variable expansion",
			config:  replaceOnce(validNativeConfig, "listen: :9400", "listen: $(ADDR):9400"),
			wantErr: "must define a Prometheus output that uses a wildcard address on port 9400",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			features, err := ValidateNativeConfig([]byte(test.config), test.options)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			if strings.Contains(test.name, "Kubernetes source") {
				assert.True(t, features.KubernetesAPI)
			}
			if strings.Contains(test.name, "loopback hostengine works") {
				assert.True(t, features.HostNetwork)
			}
			if strings.Contains(test.name, "DRA source with") {
				assert.True(t, features.DRA)
			}
			if strings.Contains(test.name, "HPC source with") {
				assert.Equal(t, "/var/lib/jobs", features.HPCJobMappingDir)
			}
		})
	}
}

func TestValidateNativeConfigAcceptsSharedDRAConfiguration(t *testing.T) {
	config, err := RenderConfig(ConfigOptions{
		MetricsFilePath:  CustomMetricsFilePath,
		PodUID:           true,
		DRA:              true,
		HPCJobMappingDir: "/var/lib/jobs",
	})
	require.NoError(t, err)

	features, err := ValidateNativeConfig(config, NativeConfigOptions{
		DRA:               true,
		MetricsConfigName: "metrics-v1",
		HPCJobMappingDir:  "/var/lib/jobs",
	})
	require.NoError(t, err)
	assert.True(t, features.DRA)
	assert.True(t, features.KubernetesAPI)
	assert.Equal(t, "/var/lib/jobs", features.HPCJobMappingDir)
}

func replaceOnce(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}
