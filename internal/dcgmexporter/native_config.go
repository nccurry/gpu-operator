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
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// NativeConfigOptions describes Operator-managed facilities available to a native config.
type NativeConfigOptions struct {
	HostNetwork       bool
	DRA               bool
	MetricsConfigName string
	HPCJobMappingDir  string
	WebConfigFilePath string
}

// NativeConfigFeatures describes workload facilities requested by a native config.
type NativeConfigFeatures struct {
	HostNetwork      bool
	KubernetesAPI    bool
	DRA              bool
	HPCJobMappingDir string
}

// NativeConfigReference contains the validated immutable ConfigMap revision and its Pod volume source.
type NativeConfigReference struct {
	Name     string
	Key      string
	Features NativeConfigFeatures
}

// WebConfigReference contains the validated Secret selected for exporter web TLS and auth.
type WebConfigReference struct {
	Name string
}

// ValidateNativeConfigSelector checks selector requirements that older CRDs may not enforce.
func ValidateNativeConfigSelector(selector *corev1.ConfigMapKeySelector) error {
	if selector == nil {
		return nil
	}
	if selector.Name == "" {
		return fmt.Errorf("dcgmExporter.nativeConfig.name is required")
	}
	if selector.Key == "" {
		return fmt.Errorf("dcgmExporter.nativeConfig.key is required")
	}
	if selector.Optional != nil && *selector.Optional {
		return fmt.Errorf("dcgmExporter.nativeConfig.optional must be false")
	}
	return nil
}

// ValidateWebConfigSelector checks selector requirements that older CRDs may not enforce.
func ValidateWebConfigSelector(selector *corev1.SecretKeySelector) error {
	if selector == nil {
		return nil
	}
	if selector.Name == "" {
		return fmt.Errorf("dcgmExporter.webConfig.name is required")
	}
	if selector.Key == "" {
		return fmt.Errorf("dcgmExporter.webConfig.key is required")
	}
	if selector.Key == "." || selector.Key == ".." || strings.ContainsAny(selector.Key, `/\\`) {
		return fmt.Errorf("dcgmExporter.webConfig.key must be a single Secret data key")
	}
	if selector.Optional != nil && *selector.Optional {
		return fmt.Errorf("dcgmExporter.webConfig.optional must be false")
	}
	return nil
}

// ResolveNativeConfig reads and validates the immutable ConfigMap revision in the Operator namespace.
func ResolveNativeConfig(ctx context.Context, reader client.Reader, namespace string, selector *corev1.ConfigMapKeySelector, options NativeConfigOptions) (*NativeConfigReference, error) {
	if selector == nil {
		return nil, nil
	}
	if err := ValidateNativeConfigSelector(selector); err != nil {
		return nil, err
	}

	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: selector.Name}
	if err := reader.Get(ctx, key, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("dcgmExporter.nativeConfig ConfigMap %q was not found in the Operator namespace", selector.Name)
		}
		return nil, fmt.Errorf("read dcgmExporter.nativeConfig ConfigMap %q in the Operator namespace: %w", selector.Name, err)
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return nil, fmt.Errorf("dcgmExporter.nativeConfig ConfigMap %q must be immutable", selector.Name)
	}
	data, found := configMap.Data[selector.Key]
	contents := []byte(data)
	if !found {
		contents, found = configMap.BinaryData[selector.Key]
	}
	if !found {
		return nil, fmt.Errorf("dcgmExporter.nativeConfig key %q was not found in ConfigMap %q", selector.Key, selector.Name)
	}
	features, err := ValidateNativeConfig(contents, options)
	if err != nil {
		return nil, err
	}
	return &NativeConfigReference{Name: selector.Name, Key: selector.Key, Features: features}, nil
}

// ResolveWebConfig verifies the selected key exists without returning or logging Secret contents.
func ResolveWebConfig(ctx context.Context, reader client.Reader, namespace string, selector *corev1.SecretKeySelector) (*WebConfigReference, error) {
	if selector == nil {
		return nil, nil
	}
	if err := ValidateWebConfigSelector(selector); err != nil {
		return nil, err
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: selector.Name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("dcgmExporter.webConfig Secret %q was not found in the Operator namespace", selector.Name)
		}
		return nil, fmt.Errorf("read dcgmExporter.webConfig Secret %q in the Operator namespace: %w", selector.Name, err)
	}
	data, found := secret.Data[selector.Key]
	if !found {
		return nil, fmt.Errorf("dcgmExporter.webConfig key %q was not found in Secret %q", selector.Key, selector.Name)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("dcgmExporter.webConfig key %q is empty in Secret %q", selector.Key, selector.Name)
	}
	return &WebConfigReference{Name: selector.Name}, nil
}

// WebConfigFilePathForKey returns the path of a key projected from the web config Secret.
func WebConfigFilePathForKey(key string) string {
	return WebConfigDirectory + "/" + key
}

// ValidateNativeConfig checks only settings that must match Operator-owned resources.
// The pinned exporter remains responsible for validating its complete config schema.
func ValidateNativeConfig(data []byte, options NativeConfigOptions) (NativeConfigFeatures, error) {
	features := NativeConfigFeatures{HostNetwork: options.HostNetwork}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil || len(config) == 0 {
		return features, fmt.Errorf("dcgmExporter.nativeConfig must contain a YAML mapping")
	}
	if !isVersionTwo(config["version"]) {
		return features, fmt.Errorf("dcgmExporter.nativeConfig.version must be 2")
	}

	metricsPort, err := DefaultMetricsPort()
	if err != nil {
		return features, err
	}
	healthPort, err := DefaultHealthPort()
	if err != nil {
		return features, err
	}
	outputs := nestedMap(config, "outputs")
	if !hasServicePrometheusOutput(outputs, metricsPort) {
		return features, fmt.Errorf("dcgmExporter.nativeConfig must define a Prometheus output that uses a wildcard address on port %d", metricsPort)
	}

	server := nestedMap(config, "server")
	webConfigFile, hasWebConfigFile := server["web_config_file"]
	if options.WebConfigFilePath != "" {
		path, ok := webConfigFile.(string)
		if !hasWebConfigFile || !ok || path != options.WebConfigFilePath {
			return features, fmt.Errorf("dcgmExporter.nativeConfig.server.web_config_file must select the projected webConfig key at %q", options.WebConfigFilePath)
		}
	} else if hasWebConfigFile {
		return features, fmt.Errorf("dcgmExporter.nativeConfig.server.web_config_file requires dcgmExporter.webConfig")
	}

	if nativeConfigUsesLoopbackHostEngine(config) && !options.HostNetwork {
		return features, fmt.Errorf("dcgmExporter.nativeConfig uses a loopback hostengine; set dcgmExporter.hostNetwork=true to enable host networking")
	}
	healthListen, ok := server["listen_health"].(string)
	if !ok || !validHealthAddress(healthListen, options.HostNetwork, healthPort) {
		return features, fmt.Errorf("dcgmExporter.nativeConfig.server.listen_health must bind the Operator health port %d for the selected networking mode", healthPort)
	}

	if err := validateNativeMetricsFiles(config, options.MetricsConfigName != ""); err != nil {
		return features, err
	}

	sources := nestedMap(config, "sources")
	features.KubernetesAPI = kubernetesSourceNeedsAPI(sources["kubernetes"])

	draSource, usesDRA := sources["dra"]
	if usesDRA && settingEnabled(draSource) {
		if !options.DRA {
			return features, fmt.Errorf("dcgmExporter.nativeConfig.sources.dra requires the GPUCluster DRA exporter")
		}
		draConfig, ok := draSource.(map[string]any)
		if !ok || draConfig["mode"] != "kubernetes" {
			return features, fmt.Errorf("dcgmExporter.nativeConfig.sources.dra.mode must be kubernetes")
		}
		features.DRA = true
	}
	if enrichmentEnabled(config, "dra") && !features.DRA {
		return features, fmt.Errorf("dcgmExporter.nativeConfig enables DRA enrichment without an available DRA source")
	}

	hpcSource, usesHPC := sources["hpc_jobs"]
	if usesHPC && settingEnabled(hpcSource) {
		hpcConfig, ok := hpcSource.(map[string]any)
		mappingDir, hasMappingDir := hpcConfig["mapping_dir"].(string)
		if !ok || !hasMappingDir || mappingDir == "" {
			return features, fmt.Errorf("dcgmExporter.nativeConfig.sources.hpc_jobs.mapping_dir must be set")
		}
		if options.HPCJobMappingDir == "" || mappingDir != options.HPCJobMappingDir {
			return features, fmt.Errorf("dcgmExporter.nativeConfig.sources.hpc_jobs.mapping_dir must match the mounted HPC job mapping directory")
		}
		features.HPCJobMappingDir = mappingDir
	}
	if enrichmentEnabled(config, "hpc_jobs") && features.HPCJobMappingDir == "" {
		return features, fmt.Errorf("dcgmExporter.nativeConfig enables HPC job enrichment without a mounted HPC job mapping source")
	}

	return features, nil
}

func isVersionTwo(value any) bool {
	switch number := value.(type) {
	case int:
		return number == 2
	case int32:
		return number == 2
	case int64:
		return number == 2
	case uint64:
		return number == 2
	case float64:
		return number == 2
	default:
		return false
	}
}

func nestedMap(parent map[string]any, key string) map[string]any {
	value, ok := parent[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func hasServicePrometheusOutput(outputs map[string]any, metricsPort int32) bool {
	prometheusOutputs := 0
	for _, value := range outputs {
		output, ok := value.(map[string]any)
		if ok && output["type"] == "prometheus" {
			prometheusOutputs++
		}
	}
	for _, value := range outputs {
		output, ok := value.(map[string]any)
		if !ok || output["type"] != "prometheus" {
			continue
		}
		listen, ok := output["listen"].(string)
		if !ok {
			// The exporter defaults one Prometheus output to :9400.
			if prometheusOutputs == 1 {
				return true
			}
			continue
		}
		if !kubernetesEnvExpansion.MatchString(listen) && metricsAddress(listen, metricsPort) {
			return true
		}
	}
	return false
}

func validHealthAddress(address string, hostNetwork bool, healthPort int32) bool {
	if hostNetwork {
		return address == "127.0.0.1:"+strconv.Itoa(int(healthPort))
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != strconv.Itoa(int(healthPort)) {
		return false
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

func validateNativeMetricsFiles(config map[string]any, customMetricsEnabled bool) error {
	metrics, _ := config["metrics"].([]any)
	foundCustomMetrics := false
	for _, metric := range metrics {
		metricConfig, ok := metric.(map[string]any)
		if !ok {
			continue
		}
		file, ok := metricConfig["file"].(string)
		if !ok {
			continue
		}
		if file == oldMetricsFilePath {
			return fmt.Errorf("dcgmExporter.nativeConfig uses the former custom metrics path %q; use %q", oldMetricsFilePath, CustomMetricsFilePath)
		}
		if file == CustomMetricsFilePath {
			if !customMetricsEnabled {
				return fmt.Errorf("dcgmExporter.nativeConfig references %q without dcgmExporter.config.name", CustomMetricsFilePath)
			}
			foundCustomMetrics = true
		}
	}
	if customMetricsEnabled && !foundCustomMetrics {
		return fmt.Errorf("dcgmExporter.nativeConfig must reference the mounted custom metrics file %q when dcgmExporter.config.name is set", CustomMetricsFilePath)
	}
	return nil
}

func kubernetesSourceNeedsAPI(value any) bool {
	source, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if enabled, ok := source["pod_uid"].(bool); ok && enabled {
		return true
	}
	if labels, ok := source["pod_labels"].(map[string]any); ok {
		if enabled, ok := labels["enabled"].(bool); ok && enabled {
			return true
		}
	}
	return false
}

func enrichmentEnabled(config map[string]any, name string) bool {
	metrics, _ := config["metrics"].([]any)
	for _, metric := range metrics {
		metricConfig, ok := metric.(map[string]any)
		if !ok {
			continue
		}
		enrichment, ok := metricConfig["enrichment"].(map[string]any)
		if ok && settingEnabled(enrichment[name]) {
			return true
		}
	}
	return false
}

func settingEnabled(value any) bool {
	switch value := value.(type) {
	case bool:
		return value
	case string:
		return value != "" && !strings.EqualFold(value, "false")
	case map[string]any:
		return len(value) > 0
	default:
		return false
	}
}

func nativeConfigUsesLoopbackHostEngine(config map[string]any) bool {
	sources := nestedMap(config, "sources")
	dcgm := nestedMap(sources, "dcgm")
	hostEngine, _ := dcgm["hostengine"].(string)
	return isLoopbackHostEngine(hostEngine)
}
