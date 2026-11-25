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

package state

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/internal/dcgmexporter"
	"github.com/NVIDIA/gpu-operator/internal/utils"
)

const (
	// dcgmExporterImageEnvName is the fallback env var for the dcgm-exporter image when the
	// CR does not specify repository/image/version.
	dcgmExporterImageEnvName = "DCGM_EXPORTER_IMAGE"

	// dcgmRemoteHostEngine points dcgm-exporter at the standalone nvidia-dcgm-dra
	// hostengine Service (manifests/state-dcgm/0600_service.yaml).
	dcgmRemoteHostEngine = "nvidia-dcgm-dra:5555"

	dcgmExporterDefaultKubeletRootDir = "/var/lib/kubelet"
)

func NewStateDCGMExporter(
	k8sClient client.Client,
	namespace string,
	scheme *runtime.Scheme,
	manifestDir string) (State, error) {

	skel, err := newStateSkel(k8sClient, namespace, scheme, manifestDir,
		"state-dcgm-exporter", "NVIDIA DCGM Exporter deployed in the cluster")
	if err != nil {
		return nil, err
	}
	return &configurableState{
		stateSkel: skel,
		isEnabled: func(cr *nvidiav1alpha1.GPUCluster) bool {
			return cr.Spec.DCGMExporter != nil && cr.Spec.DCGMExporter.IsEnabled()
		},
		imageOverride: func(cr *nvidiav1alpha1.GPUCluster) (string, string, string) {
			spec := cr.Spec.DCGMExporter
			return spec.Repository, spec.Image, spec.Version
		},
		imageEnvName:    dcgmExporterImageEnvName,
		buildRenderData: buildDCGMExporterRenderData,
	}, nil
}

func buildDCGMExporterRenderData(ctx context.Context, s *configurableState, cr *nvidiav1alpha1.GPUCluster, imagePath, apiVersion, openshiftVersion string) (any, error) {
	spec := cr.Spec.DCGMExporter
	if err := dcgmexporter.ValidateUserOverrides(spec.Args, spec.Env); err != nil {
		return nil, err
	}

	// When standalone DCGM is enabled the exporter targets it; otherwise it runs embedded.
	remoteHostEngine := ""
	if dcgmEnabled(cr) {
		remoteHostEngine = dcgmRemoteHostEngine
	}

	metricsConfigName := ""
	if spec.MetricsConfig != nil && spec.MetricsConfig.Name != "" {
		metricsConfigName = spec.MetricsConfig.Name
	}

	kubeletRootDir := cr.Spec.HostPaths.KubeletRootDir
	if kubeletRootDir == "" {
		kubeletRootDir = dcgmExporterDefaultKubeletRootDir
	}

	// Skip the ServiceMonitor when its CRD is absent so a default install without the
	// Prometheus Operator does not fail (matches the ClusterPolicy path).
	serviceMonitorEnabled := spec.ServiceMonitor != nil &&
		spec.ServiceMonitor.Enabled != nil && *spec.ServiceMonitor.Enabled
	if serviceMonitorEnabled && !serviceMonitorCRDServed(s.client) {
		log.FromContext(ctx).V(consts.LogLevelInfo).Info(
			"ServiceMonitor CRD not served; skipping dcgm-exporter ServiceMonitor creation")
		serviceMonitorEnabled = false
	}

	configOptions := dcgmexporter.ConfigOptionsFromSpec(spec, remoteHostEngine, spec.IsHostNetworkEnabled(), true)
	if err := dcgmexporter.ValidateWebConfigOverride(spec.Args, spec.Env, configOptions.WebConfigFilePath); err != nil {
		return nil, err
	}
	webConfig, err := dcgmexporter.ResolveWebConfig(ctx, s.client, s.namespace, spec.WebConfig)
	if err != nil {
		return nil, err
	}
	var nativeConfig *dcgmexporter.NativeConfigReference
	if spec.NativeConfig != nil {
		nativeOptions := dcgmexporter.NativeConfigOptions{
			HostNetwork:       configOptions.HostNetwork,
			DRA:               true,
			MetricsConfigName: metricsConfigName,
			HPCJobMappingDir:  configOptions.HPCJobMappingDir,
			WebConfigFilePath: configOptions.WebConfigFilePath,
		}
		nativeConfig, err = dcgmexporter.ResolveNativeConfig(ctx, s.client, s.namespace, spec.NativeConfig, nativeOptions)
		if err != nil {
			return nil, err
		}
		configOptions.HostNetwork = nativeConfig.Features.HostNetwork
	}
	containerDefaults := &corev1.Container{}
	if err := dcgmexporter.ApplyDeploymentDefaults(containerDefaults, configOptions.HostNetwork); err != nil {
		return nil, err
	}
	generatedConfig, err := dcgmexporter.RenderConfig(configOptions)
	if err != nil {
		return nil, err
	}
	podAnnotations := make(map[string]string, len(cr.Spec.Daemonsets.Annotations)+len(spec.Annotations)+1)
	maps.Copy(podAnnotations, cr.Spec.Daemonsets.Annotations)
	maps.Copy(podAnnotations, spec.Annotations)
	if nativeConfig != nil {
		if _, exists := podAnnotations[dcgmexporter.ConfigAnnotation]; exists {
			return nil, fmt.Errorf("pod template annotation %q is reserved for the generated dcgm-exporter configuration", dcgmexporter.ConfigAnnotation)
		}
	} else {
		if err := dcgmexporter.ValidateConfigAnnotation(generatedConfig, podAnnotations); err != nil {
			return nil, err
		}
		podAnnotations[dcgmexporter.ConfigAnnotation] = string(generatedConfig)
	}

	serviceType := "ClusterIP"
	serviceInternalTrafficPolicy := ""
	if spec.ServiceSpec != nil {
		if spec.ServiceSpec.Type != "" {
			serviceType = string(spec.ServiceSpec.Type)
		}
		if spec.ServiceSpec.InternalTrafficPolicy != nil {
			serviceInternalTrafficPolicy = string(*spec.ServiceSpec.InternalTrafficPolicy)
		}
	}

	daemonsets := cr.Spec.Daemonsets
	updateStrategy, err := dcgmExporterUpdateStrategy(&daemonsets)
	if err != nil {
		return nil, err
	}
	renderData := &dcgmExporterRenderData{
		DCGMExporter:                 &dcgmExporterSpec{Spec: spec, ImagePath: imagePath},
		Daemonsets:                   &daemonsets,
		UpdateStrategy:               updateStrategy,
		Namespace:                    s.namespace,
		OpenshiftVersion:             openshiftVersion,
		ResourceClaimAPIVersion:      apiVersion,
		ConfigFileEnvName:            dcgmexporter.ConfigFileEnvName,
		ConfigFileDirectory:          dcgmexporter.ConfigFileDirectory,
		ConfigFileName:               dcgmexporter.ConfigFileName,
		ConfigFilePath:               dcgmexporter.ConfigFilePath,
		ConfigVolumeName:             dcgmexporter.ConfigVolumeName,
		ConfigAnnotation:             dcgmexporter.ConfigAnnotation,
		PodAnnotations:               podAnnotations,
		HPCJobMappingDir:             configOptions.HPCJobMappingDir,
		HostPID:                      spec.IsHostPIDEnabled(),
		HostNetwork:                  configOptions.HostNetwork,
		MetricsConfigName:            metricsConfigName,
		MetricsConfigMountDirectory:  filepath.Dir(dcgmexporter.CustomMetricsFilePath),
		ContainerDefaults:            containerDefaults,
		ServiceMonitorEnabled:        serviceMonitorEnabled,
		PodResourcesDir:              filepath.Join(kubeletRootDir, "pod-resources"),
		ServiceType:                  serviceType,
		ServiceInternalTrafficPolicy: serviceInternalTrafficPolicy,
	}
	if nativeConfig != nil {
		renderData.NativeConfigName = nativeConfig.Name
		renderData.NativeConfigKey = nativeConfig.Key
	}
	if webConfig != nil {
		renderData.WebConfigSecretName = webConfig.Name
		renderData.WebConfigMountDirectory = dcgmexporter.WebConfigDirectory
	}
	return renderData, nil
}

func dcgmExporterUpdateStrategy(daemonsets *nvidiav1.DaemonsetsSpec) (appsv1.DaemonSetUpdateStrategy, error) {
	if daemonsets.UpdateStrategy == "OnDelete" {
		return appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}, nil
	}

	strategy := appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType}
	if daemonsets.RollingUpdate == nil || daemonsets.RollingUpdate.MaxUnavailable == "" {
		return strategy, nil
	}

	value, err := utils.ParseMaxUnavailable(daemonsets.RollingUpdate.MaxUnavailable)
	if err != nil {
		return appsv1.DaemonSetUpdateStrategy{}, fmt.Errorf("failed to apply rolling update config: %s", err)
	}
	strategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &value}

	return strategy, nil
}

// serviceMonitorCRDServed reports whether the cluster serves the monitoring.coreos.com
// ServiceMonitor kind (i.e. the Prometheus Operator CRDs are installed).
func serviceMonitorCRDServed(k8sClient client.Client) bool {
	_, err := k8sClient.RESTMapper().RESTMapping(
		schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}, "v1")
	return err == nil
}
