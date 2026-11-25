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

// Package dcgmexporter renders the configuration shared by both GPU Operator
// deployment paths.
package dcgmexporter

import (
	"fmt"
	"maps"
	"net"
	"strings"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/yaml"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

const (
	// ConfigFileEnvName selects the generated configuration file in DCGM Exporter.
	ConfigFileEnvName = "DCGM_EXPORTER_CONFIG_FILE"
	// ConfigFileDirectory is the mount directory for the generated configuration.
	ConfigFileDirectory = "/etc/dcgm-exporter/operator/config"
	// ConfigFileName is the generated configuration file projected into the container.
	ConfigFileName = "dcgm-exporter.yaml"
	// ConfigFilePath is the full path of the generated configuration file.
	ConfigFilePath = ConfigFileDirectory + "/" + ConfigFileName
	// ConfigVolumeName is the name of the generated configuration volume.
	ConfigVolumeName = "dcgm-exporter-config"
	// ConfigAnnotation carries the generated exporter configuration for DownwardAPI projection.
	ConfigAnnotation = "nvidia.com/dcgm-exporter-config"
	// WebConfigDirectory is the mount directory for the exporter web configuration and its certificates.
	WebConfigDirectory = "/etc/dcgm-exporter/operator/web-config"
	// WebConfigVolumeName names the Secret volume for exporter web TLS and authentication.
	WebConfigVolumeName = "dcgm-exporter-web-config"
	// DefaultMetricsFilePath is the default metrics list included in the exporter image.
	DefaultMetricsFilePath = "/etc/dcgm-exporter/default-counters.csv"
	// CustomMetricsFilePath is the mounted location of a user-supplied metrics list.
	CustomMetricsFilePath = "/etc/dcgm-exporter/operator/metrics/dcgm-metrics.csv"
	// HealthPortName is the named port used by the exporter HTTP probes.
	HealthPortName = "health"
)

const (
	metricsType         = "prometheus"
	metricsSchema       = "dcgm/v1"
	oldMetricsFilePath  = "/etc/dcgm-exporter/dcgm-metrics.csv"
	remoteHostEngineEnv = "DCGM_REMOTE_HOSTENGINE_INFO"
	podLabelsEnvName    = "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_LABELS"
	podUIDEnvName       = "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_UID"
)

// ConfigOptions contains settings that the Operator owns in exporter config.
type ConfigOptions struct {
	MetricsFilePath   string
	WebConfigFilePath string
	HostEngine        string
	HostNetwork       bool
	PodUID            bool
	PodLabels         bool
	PodLabelAllow     []string
	DRA               bool
	HPCJobMappingDir  string
}

// RenderConfig returns a v2 exporter configuration file.
func RenderConfig(options ConfigOptions) ([]byte, error) {
	defaults, err := DefaultDeploymentDefaults()
	if err != nil {
		return nil, err
	}
	metricsPort, err := defaults.Port(MetricsPortName)
	if err != nil {
		return nil, err
	}
	healthPort, err := defaults.Port(HealthPortName)
	if err != nil {
		return nil, err
	}

	metricsFile := options.MetricsFilePath
	if metricsFile == "" {
		metricsFile = DefaultMetricsFilePath
	}

	draEnrichment := any(false)
	if options.DRA {
		draEnrichment = "auto"
	}
	hpcEnrichment := any(false)
	if options.HPCJobMappingDir != "" {
		hpcEnrichment = "auto"
	}

	allow := append([]string{}, options.PodLabelAllow...)
	if options.PodLabels && len(allow) == 0 {
		allow = []string{".*"}
	}

	dcgmSource := map[string]any{}
	if options.HostEngine != "" {
		dcgmSource["hostengine"] = options.HostEngine
	}
	sources := map[string]any{}
	if len(dcgmSource) > 0 {
		sources["dcgm"] = dcgmSource
	}
	if options.PodUID || options.PodLabels {
		sources["kubernetes"] = map[string]any{
			"pod_uid": options.PodUID,
			"pod_labels": map[string]any{
				"enabled": options.PodLabels,
				"allow":   allow,
			},
		}
	}
	if options.DRA {
		sources["dra"] = map[string]any{"mode": "kubernetes"}
	}
	if options.HPCJobMappingDir != "" {
		sources["hpc_jobs"] = map[string]any{"mapping_dir": options.HPCJobMappingDir}
	}

	healthListen := fmt.Sprintf(":%d", healthPort)
	if options.HostNetwork {
		healthListen = fmt.Sprintf("127.0.0.1:%d", healthPort)
	}
	metricsListenAddress := fmt.Sprintf(":%d", metricsPort)
	server := map[string]any{"listen_health": healthListen}
	if options.WebConfigFilePath != "" {
		server["web_config_file"] = options.WebConfigFilePath
	}
	config := map[string]any{
		"version": 2,
		"metrics": []any{
			map[string]any{
				"name":  "default",
				"file":  metricsFile,
				"every": "30s",
				"enrichment": map[string]any{
					"kubernetes": "auto",
					"container":  false,
					"dra":        draEnrichment,
					"hpc_jobs":   hpcEnrichment,
				},
			},
		},
		"outputs": map[string]any{
			"prometheus": map[string]any{
				"type":   metricsType,
				"schema": metricsSchema,
				"listen": metricsListenAddress,
			},
		},
		"server": server,
	}
	if len(sources) > 0 {
		config["sources"] = sources
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal dcgm-exporter configuration: %w", err)
	}
	return data, nil
}

// ValidateConfigAnnotation checks that the generated configuration and existing Pod
// annotations fit Kubernetes' annotation size limit without claiming a user annotation.
func ValidateConfigAnnotation(config []byte, annotations map[string]string) error {
	if value, exists := annotations[ConfigAnnotation]; exists && value != string(config) {
		return fmt.Errorf("pod template annotation %q is reserved for the generated dcgm-exporter configuration", ConfigAnnotation)
	}

	combined := make(map[string]string, len(annotations)+1)
	maps.Copy(combined, annotations)
	combined[ConfigAnnotation] = string(config)
	if err := apivalidation.ValidateAnnotationsSize(combined); err != nil {
		return fmt.Errorf("generated dcgm-exporter configuration and other Pod annotations exceed Kubernetes' %d-byte annotation limit; reduce exporter settings or other Pod annotations: %w", apivalidation.TotalAnnotationSizeLimitB, err)
	}

	return nil
}

// ConfigOptionsFromSpec maps supported Operator fields into the shared v2 config.
// Legacy args and environment variables remain on the container and keep their
// exporter-defined precedence.
func ConfigOptionsFromSpec(spec *gpuv1.DCGMExporterSpec, hostEngine string, hostNetwork, dra bool) ConfigOptions {
	options := ConfigOptions{
		MetricsFilePath: DefaultMetricsFilePath,
		HostEngine:      hostEngine,
		HostNetwork:     hostNetwork,
		DRA:             dra,
	}
	if spec == nil {
		return options
	}
	if spec.MetricsConfig != nil && spec.MetricsConfig.Name != "" {
		options.MetricsFilePath = CustomMetricsFilePath
	}
	if spec.WebConfig != nil && spec.WebConfig.Name != "" && spec.WebConfig.Key != "" {
		options.WebConfigFilePath = WebConfigFilePathForKey(spec.WebConfig.Key)
	}
	options.PodUID = PodUIDEnabled(spec)
	options.PodLabels = PodLabelsEnabled(spec)
	options.PodLabelAllow = append([]string(nil), spec.PodLabelAllowlistRegex...)
	if spec.IsHPCJobMappingEnabled() {
		options.HPCJobMappingDir = spec.GetHPCJobMappingDirectory()
		if options.HPCJobMappingDir == "" {
			options.HPCJobMappingDir = gpuv1.DefaultDCGMJobMappingDir
		}
	}

	remoteHostEngine := hostEngine
	if value, _, explicit := effectiveStringOption(spec.Args, spec.Env, remoteHostEngineEnv, "--remote-hostengine-info", "-remote-hostengine-info", "--r", "-r"); explicit {
		remoteHostEngine = value
	}
	if isLoopbackHostEngine(remoteHostEngine) {
		options.HostNetwork = true
	}
	return options
}

// PodUIDEnabled returns whether the effective exporter settings enable pod UID enrichment.
// Command-line flags take precedence over environment variables, which take
// precedence over the typed Operator field.
func PodUIDEnabled(spec *gpuv1.DCGMExporterSpec) bool {
	if spec == nil {
		return false
	}
	return effectiveBoolOption(spec.IsPodUIDEnabled(), spec.Args, spec.Env, podUIDEnvName, "--kubernetes-enable-pod-uid", "-kubernetes-enable-pod-uid")
}

// PodLabelsEnabled returns whether the effective exporter settings enable pod-label enrichment.
// Command-line flags take precedence over environment variables, which take
// precedence over the typed Operator field.
func PodLabelsEnabled(spec *gpuv1.DCGMExporterSpec) bool {
	if spec == nil {
		return false
	}
	return effectiveBoolOption(spec.IsPodLabelsEnabled(), spec.Args, spec.Env, podLabelsEnvName, "--kubernetes-enable-pod-labels", "-kubernetes-enable-pod-labels")
}

// PodMetadataEnabled reports whether either effective pod metadata option is enabled.
func PodMetadataEnabled(spec *gpuv1.DCGMExporterSpec) bool {
	return PodUIDEnabled(spec) || PodLabelsEnabled(spec)
}

func isLoopbackHostEngine(address string) bool {
	if scheme, rest, ok := strings.Cut(address, "://"); ok {
		if scheme != "tcp" {
			return false
		}
		address = rest
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
