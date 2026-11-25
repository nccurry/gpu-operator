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
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/yaml"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

func TestRenderConfigDefaults(t *testing.T) {
	configYAML, err := RenderConfig(ConfigOptions{})
	require.NoError(t, err)

	config := parseConfig(t, configYAML)
	assert.Equal(t, float64(2), config["version"])
	assert.NotContains(t, config, "sources", "embedded mode does not need an explicit DCGM source")

	metrics := firstMetric(t, config)
	assert.Equal(t, DefaultMetricsFilePath, metrics["file"])
	assert.Equal(t, "30s", metrics["every"])
	enrichment := metrics["enrichment"].(map[string]any)
	assert.Equal(t, "auto", enrichment["kubernetes"])
	assert.Equal(t, false, enrichment["container"])
	assert.Equal(t, false, enrichment["dra"])
	assert.Equal(t, false, enrichment["hpc_jobs"])

	outputs := config["outputs"].(map[string]any)
	prometheus := outputs["prometheus"].(map[string]any)
	assert.Equal(t, metricsType, prometheus["type"])
	assert.Equal(t, metricsSchema, prometheus["schema"])
	assert.Equal(t, defaultPortAddress(t, "", MetricsPortName), prometheus["listen"])
	assert.Equal(t, defaultPortAddress(t, "", HealthPortName), config["server"].(map[string]any)["listen_health"])
}

func TestRenderConfigUsesOperatorInputs(t *testing.T) {
	configYAML, err := RenderConfig(ConfigOptions{
		MetricsFilePath:  CustomMetricsFilePath,
		HostEngine:       "nvidia-dcgm:5555",
		HostNetwork:      true,
		PodUID:           true,
		PodLabels:        true,
		DRA:              true,
		HPCJobMappingDir: "/var/lib/jobs",
	})
	require.NoError(t, err)

	config := parseConfig(t, configYAML)
	metrics := firstMetric(t, config)
	assert.Equal(t, CustomMetricsFilePath, metrics["file"])
	enrichment := metrics["enrichment"].(map[string]any)
	assert.Equal(t, "auto", enrichment["dra"])
	assert.Equal(t, "auto", enrichment["hpc_jobs"])

	sources := config["sources"].(map[string]any)
	assert.Equal(t, "nvidia-dcgm:5555", sources["dcgm"].(map[string]any)["hostengine"])
	assert.Equal(t, map[string]any{"mode": "kubernetes"}, sources["dra"])
	assert.Equal(t, map[string]any{"mapping_dir": "/var/lib/jobs"}, sources["hpc_jobs"])
	kubernetes := sources["kubernetes"].(map[string]any)
	assert.Equal(t, true, kubernetes["pod_uid"])
	assert.Equal(t, map[string]any{"enabled": true, "allow": []any{".*"}}, kubernetes["pod_labels"])
	assert.Equal(t, defaultPortAddress(t, "127.0.0.1", HealthPortName), config["server"].(map[string]any)["listen_health"])
}

func TestConfigOptionsFromSpecPreservesOverrides(t *testing.T) {
	spec := &gpuv1.DCGMExporterSpec{
		EnablePodLabels:        new(true),
		PodLabelAllowlistRegex: []string{"^app$"},
		Args:                   []string{"-r", "localhost:5555"},
		Env: []gpuv1.EnvVar{
			{Name: remoteHostEngineEnv, Value: "nvidia-dcgm:5555"},
		},
	}

	options := ConfigOptionsFromSpec(spec, "nvidia-dcgm:5555", false, true)
	assert.True(t, options.PodLabels)
	assert.False(t, options.PodUID)
	assert.Equal(t, []string{"^app$"}, options.PodLabelAllow)
	assert.True(t, options.HostNetwork, "a loopback hostengine must use the host network")
	assert.Equal(t, "nvidia-dcgm:5555", options.HostEngine, "user hostengine settings remain exporter-owned")
}

func TestConfigOptionsRecognizeSingleDashLongHostEngineFlag(t *testing.T) {
	spec := &gpuv1.DCGMExporterSpec{
		Args: []string{"-remote-hostengine-info", "localhost:5555"},
	}
	options := ConfigOptionsFromSpec(spec, "nvidia-dcgm:5555", false, false)
	assert.True(t, options.HostNetwork, "the released CLI accepts the single-dash long flag")
}

func TestConfigOptionsRecognizeDoubleDashRemoteAlias(t *testing.T) {
	spec := &gpuv1.DCGMExporterSpec{
		Args: []string{"--r", "localhost:5555"},
	}
	options := ConfigOptionsFromSpec(spec, "nvidia-dcgm:5555", false, false)
	assert.True(t, options.HostNetwork, "a loopback hostengine passed with --r needs host networking")
}

func TestPodMetadataOptionsUseExporterPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		spec      gpuv1.DCGMExporterSpec
		wantUID   bool
		wantLabel bool
	}{
		{
			name:      "typed fields",
			spec:      gpuv1.DCGMExporterSpec{EnablePodUID: new(true), EnablePodLabels: new(true)},
			wantUID:   true,
			wantLabel: true,
		},
		{
			name: "argument false overrides typed true",
			spec: gpuv1.DCGMExporterSpec{
				EnablePodLabels: new(true),
				Args:            []string{"--kubernetes-enable-pod-labels=false"},
			},
		},
		{
			name: "argument true overrides environment false",
			spec: gpuv1.DCGMExporterSpec{
				Env:  []gpuv1.EnvVar{{Name: podUIDEnvName, Value: "false"}},
				Args: []string{"--kubernetes-enable-pod-uid"},
			},
			wantUID: true,
		},
		{
			name: "environment false overrides typed true",
			spec: gpuv1.DCGMExporterSpec{
				EnablePodUID: new(true),
				Env:          []gpuv1.EnvVar{{Name: podUIDEnvName, Value: "false"}},
			},
		},
		{
			name: "environment one enables labels",
			spec: gpuv1.DCGMExporterSpec{
				Env: []gpuv1.EnvVar{{Name: podLabelsEnvName, Value: "1"}},
			},
			wantLabel: true,
		},
		{
			name: "argument one enables labels",
			spec: gpuv1.DCGMExporterSpec{
				Args: []string{"--kubernetes-enable-pod-labels=1"},
			},
			wantLabel: true,
		},
		{
			name: "argument zero overrides environment true",
			spec: gpuv1.DCGMExporterSpec{
				Env:  []gpuv1.EnvVar{{Name: podLabelsEnvName, Value: "true"}},
				Args: []string{"-kubernetes-enable-pod-labels=0"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.wantUID, PodUIDEnabled(&test.spec))
			assert.Equal(t, test.wantLabel, PodLabelsEnabled(&test.spec))
			assert.Equal(t, test.wantUID || test.wantLabel, PodMetadataEnabled(&test.spec))
		})
	}
}

func TestEffectiveStringOptionAliasesAndPrecedence(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		env     []gpuv1.EnvVar
		flags   []string
		envName string
		want    string
		set     bool
	}{
		{
			name:    "remote shorthand equals",
			args:    []string{"-r=localhost:5555"},
			envName: remoteHostEngineEnv,
			flags:   []string{"--remote-hostengine-info", "-remote-hostengine-info", "--r", "-r"},
			want:    "localhost:5555",
			set:     true,
		},
		{
			name:    "remote long single dash",
			args:    []string{"-remote-hostengine-info", "localhost:5555"},
			envName: remoteHostEngineEnv,
			flags:   []string{"--remote-hostengine-info", "-remote-hostengine-info", "--r", "-r"},
			want:    "localhost:5555",
			set:     true,
		},
		{
			name:    "argument overrides environment",
			args:    []string{"-a", ":9400"},
			env:     []gpuv1.EnvVar{{Name: metricsListenEnvName, Value: "127.0.0.1:9400"}},
			envName: metricsListenEnvName,
			flags:   []string{"--address", "-a"},
			want:    ":9400",
			set:     true,
		},
		{
			name:    "collectors shorthand equals",
			args:    []string{"-f=" + CustomMetricsFilePath},
			envName: metricsCollectorsEnvName,
			flags:   []string{"--collectors", "-f"},
			want:    CustomMetricsFilePath,
			set:     true,
		},
		{
			name:    "stop at delimiter",
			args:    []string{"--", "-r", "localhost:5555"},
			envName: remoteHostEngineEnv,
			flags:   []string{"--remote-hostengine-info", "-remote-hostengine-info", "--r", "-r"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _, set := effectiveStringOption(test.args, test.env, test.envName, test.flags...)
			assert.Equal(t, test.set, set)
			if test.set {
				assert.Equal(t, test.want, got)
			}
		})
	}
}

func TestValidateConfigAnnotationSize(t *testing.T) {
	maxConfigBytes := apivalidation.TotalAnnotationSizeLimitB - len(ConfigAnnotation)
	config := []byte(strings.Repeat("x", maxConfigBytes))
	require.NoError(t, ValidateConfigAnnotation(config, nil))

	tooLarge := append([]byte("config-marker"), []byte(strings.Repeat("x", maxConfigBytes-len("config-marker")+1))...)
	err := ValidateConfigAnnotation(tooLarge, map[string]string{"team": "gpu"})
	require.ErrorContains(t, err, "reduce exporter settings or other Pod annotations")
	assert.NotContains(t, err.Error(), "config-marker", "errors must not include generated config contents")
}

func TestValidateConfigAnnotationReservesItsKey(t *testing.T) {
	err := ValidateConfigAnnotation([]byte("version: 2\n"), map[string]string{ConfigAnnotation: "user-value"})
	require.ErrorContains(t, err, "reserved")
	assert.NotContains(t, err.Error(), "user-value")
}

func TestValidateUserOverrides(t *testing.T) {
	metricsPort, err := DefaultMetricsPort()
	require.NoError(t, err)
	metricsLoopbackAddress := defaultPortAddress(t, "127.0.0.1", MetricsPortName)
	tests := map[string]struct {
		args []string
		env  []gpuv1.EnvVar
		want string
	}{
		"generated selector": {
			args: []string{"--config-file", "/etc/alternate.yaml"},
			want: "--config-file",
		},
		"generated selector equals form": {
			args: []string{"--config-file=/etc/alternate.yaml"},
			want: "--config-file",
		},
		"generated selector single dash form": {
			args: []string{"-config-file=/etc/alternate.yaml"},
			want: "-config-file",
		},
		"generated selector environment even when empty": {
			env:  []gpuv1.EnvVar{{Name: ConfigFileEnvName}},
			want: ConfigFileEnvName,
		},
		"listener conflicts with Service": {
			args: []string{"--address", fmt.Sprintf(":%d", metricsPort+1)},
			want: "--address",
		},
		"address short alias with incompatible port": {
			args: []string{"--a", ":9500"},
			want: "--a",
		},
		"listener host conflicts with Service": {
			env:  []gpuv1.EnvVar{{Name: metricsListenEnvName, Value: metricsLoopbackAddress}},
			want: metricsListenEnvName,
		},
		"expanded listener with wrong numeric port": {
			args: []string{"--address", fmt.Sprintf("$(EXPORTER_HOST):%d", metricsPort+1)},
			want: "--address",
		},
		"expanded listener with non-numeric port": {
			args: []string{"--address", "$(EXPORTER_HOST):invalid"},
			want: "--address",
		},
		"expanded listener in environment": {
			env:  []gpuv1.EnvVar{{Name: metricsListenEnvName, Value: "$(EXPORTER_LISTEN_ADDRESS)"}},
			want: metricsListenEnvName,
		},
		"CLI expansion with a conflicting literal port": {
			args: []string{"--address", "$(METRICS_HOST):1234"},
			want: "--address",
		},
		"former CSV path needs migration": {
			args: []string{"-f", oldMetricsFilePath},
			want: CustomMetricsFilePath,
		},
		"double dash collectors alias needs migration": {
			args: []string{"--f", oldMetricsFilePath},
			want: CustomMetricsFilePath,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateUserOverrides(test.args, test.env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestValidateUserOverridesAllowsCompatibleOptions(t *testing.T) {
	metricsPort, err := DefaultMetricsPort()
	require.NoError(t, err)
	metricsAddress := defaultPortAddress(t, "", MetricsPortName)
	metricsIPv4Address := defaultPortAddress(t, "0.0.0.0", MetricsPortName)
	metricsIPv6Address := defaultPortAddress(t, "::", MetricsPortName)
	for _, test := range []struct {
		name string
		args []string
		env  []gpuv1.EnvVar
	}{
		{name: "no overrides"},
		{name: "metrics wildcard listener", args: []string{"--address", metricsAddress}},
		{name: "metrics IPv4 wildcard listener", env: []gpuv1.EnvVar{{Name: metricsListenEnvName, Value: metricsIPv4Address}}},
		{name: "metrics IPv6 wildcard listener", args: []string{"-a", metricsIPv6Address}},
		{name: "Kubernetes-expanded listener", args: []string{"--address", "$(EXPORTER_LISTEN_ADDRESS)"}},
		{name: "Kubernetes-expanded listener equals form", args: []string{"--address=$(EXPORTER_LISTEN_ADDRESS)"}},
		{name: "Kubernetes-expanded host with Service port", args: []string{"--address", fmt.Sprintf("$(EXPORTER_HOST):%d", metricsPort)}},
		{name: "Kubernetes-expanded port with wildcard host", args: []string{"--address", "0.0.0.0:$(EXPORTER_PORT)"}},
		{name: "operator metrics path", args: []string{"--collectors=" + CustomMetricsFilePath}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, ValidateUserOverrides(test.args, test.env))
		})
	}
}

func TestServiceAddressRequiresLiteralWildcardAndPort(t *testing.T) {
	metricsPort, err := DefaultMetricsPort()
	require.NoError(t, err)
	for _, address := range []string{"$(EXPORTER_LISTEN_ADDRESS)", fmt.Sprintf("$(EXPORTER_HOST):%d", metricsPort)} {
		assert.False(t, serviceAddress(address, metricsPort), "serviceAddress must only validate literal listeners")
	}
	assert.True(t, serviceAddress(fmt.Sprintf(":%d", metricsPort), metricsPort))
}

func TestServiceAddressUsesConfiguredPort(t *testing.T) {
	const configuredPort = 12345

	assert.True(t, serviceAddress(":12345", configuredPort))
	assert.True(t, serviceAddress("0.0.0.0:12345", configuredPort))
	assert.True(t, serviceAddress("[::]:12345", configuredPort))
	assert.False(t, serviceAddress(":9400", configuredPort))
	assert.False(t, serviceAddress("$(EXPORTER_PORT)", configuredPort))
}

func defaultPortAddress(t *testing.T, host, name string) string {
	t.Helper()
	defaults, err := DefaultDeploymentDefaults()
	require.NoError(t, err)
	port, err := defaults.Port(name)
	require.NoError(t, err)
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

func parseConfig(t *testing.T, content []byte) map[string]any {
	t.Helper()
	var config map[string]any
	require.NoError(t, yaml.Unmarshal(content, &config))
	return config
}

func firstMetric(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	metrics, ok := config["metrics"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, metrics)
	return metrics[0].(map[string]any)
}
