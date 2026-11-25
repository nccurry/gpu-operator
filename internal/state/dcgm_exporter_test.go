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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/dcgmexporter"
)

const dcgmExporterManifestDir = "../../manifests/state-dcgm-exporter"

// restMapperWithServiceMonitor returns a RESTMapper that serves the ServiceMonitor kind when
// present is true, and an empty one otherwise (simulating a cluster without the Prometheus Operator).
func restMapperWithServiceMonitor(present bool) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "monitoring.coreos.com", Version: "v1"}})
	if present {
		m.Add(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
			meta.RESTScopeNamespace)
	}
	return m
}

func newTestDCGMExporterState(t *testing.T, serviceMonitorCRD bool) *configurableState {
	t.Helper()
	t.Setenv("DCGM_EXPORTER_IMAGE", "nvcr.io/nvidia/k8s/dcgm-exporter:test")
	client := fake.NewClientBuilder().
		WithRESTMapper(restMapperWithServiceMonitor(serviceMonitorCRD)).
		Build()
	s, err := NewStateDCGMExporter(client, "test-operator", runtime.NewScheme(), dcgmExporterManifestDir)
	require.NoError(t, err)
	return s.(*configurableState)
}

// exporterCR returns a sample CR with dcgm-exporter enabled and the given exporter spec.
func exporterCR(spec *nvidiav1.DCGMExporterSpec) *nvidiav1alpha1.GPUCluster {
	cr := sampleGPUCluster()
	cr.Spec.DCGMExporter = spec
	return cr
}

func TestDCGMExporterEnabledByDefault(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	// Enabled nil -> enabled by default for the exporter.
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{Repository: "nvcr.io/nvidia/k8s", Image: "dcgm-exporter", Version: "4.5.3"})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	kinds := kindCounts(objs)
	assert.Equal(t, 1, kinds["ServiceAccount"])
	assert.Equal(t, 1, kinds["Role"])
	assert.Equal(t, 1, kinds["RoleBinding"])
	assert.Equal(t, 1, kinds["ResourceClaimTemplate"])
	assert.Equal(t, 1, kinds["DaemonSet"])
	assert.Equal(t, 1, kinds["Service"])
	// DRA attribution needs the ResourceSlice informer, so the read ClusterRole is
	// always bound. No ServiceMonitor by default.
	assert.Equal(t, 1, kinds["ClusterRole"])
	assert.Equal(t, 1, kinds["ClusterRoleBinding"])
	assert.Equal(t, 0, kinds["ServiceMonitor"])

	claimHasAdminAccess(t, findByKind(objs, "ResourceClaimTemplate"))

	ds := findDaemonSet(t, objs)
	podSpec := ds.Spec.Template.Spec
	assert.Equal(t, "true", podSpec.NodeSelector["nvidia.com/gpu.deploy.dcgm-exporter-dra"])
	require.NotNil(t, podSpec.AutomountServiceAccountToken)
	assert.True(t, *podSpec.AutomountServiceAccountToken)

	ctr := podSpec.Containers[0]
	defaults, err := dcgmexporter.DefaultDeploymentDefaults()
	require.NoError(t, err)
	metricsPort, err := defaults.Port(dcgmexporter.MetricsPortName)
	require.NoError(t, err)
	healthPort, err := defaults.Port(dcgmexporter.HealthPortName)
	require.NoError(t, err)
	assert.Equal(t, "nvcr.io/nvidia/k8s/dcgm-exporter:4.5.3", ctr.Image)
	env := envMap(ctr.Env)
	assert.Equal(t, dcgmexporter.ConfigFilePath, env[dcgmexporter.ConfigFileEnvName])
	assert.NotContains(t, env, "DCGM_EXPORTER_LISTEN")
	assert.NotContains(t, env, "DCGM_EXPORTER_COLLECTORS")
	assert.NotContains(t, env, "KUBERNETES_ENABLE_DRA")
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_LABELS")
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_UID")
	config := exporterConfig(t, objs)
	assert.Contains(t, config, "file: /etc/dcgm-exporter/default-counters.csv")
	assert.Contains(t, config, "mode: kubernetes")
	assert.Equal(t, config, ds.Spec.Template.Annotations[dcgmexporter.ConfigAnnotation])
	configVolume := findVolume(t, ds, dcgmexporter.ConfigVolumeName)
	require.NotNil(t, configVolume.DownwardAPI)
	require.Len(t, configVolume.DownwardAPI.Items, 1)
	assert.Equal(t, dcgmexporter.ConfigFileName, configVolume.DownwardAPI.Items[0].Path)
	require.NotNil(t, configVolume.DownwardAPI.Items[0].FieldRef)
	assert.Equal(t, "metadata.annotations['"+dcgmexporter.ConfigAnnotation+"']", configVolume.DownwardAPI.Items[0].FieldRef.FieldPath)
	assert.Nil(t, configVolume.ConfigMap)
	// Embedded engine: no remote host-engine env.
	_, hasRemote := env["DCGM_REMOTE_HOSTENGINE_INFO"]
	assert.False(t, hasRemote, "exporter must run embedded when standalone DCGM is disabled")
	assert.NotContains(t, ds.Spec.Template.Annotations, "nvidia.com/dcgm-exporter-config-hash")
	assert.Equal(t, defaults.Ports, ctr.Ports)
	assert.Equal(t, metricsPort, ctr.Ports[0].ContainerPort)
	assert.Equal(t, healthPort, ctr.Ports[1].ContainerPort)
	assert.Equal(t, defaults.StartupProbe, ctr.StartupProbe)
	assert.Equal(t, defaults.LivenessProbe, ctr.LivenessProbe)
	assert.Equal(t, defaults.ReadinessProbe, ctr.ReadinessProbe)
	assert.Equal(t, dcgmexporter.HealthPortName, ctr.ReadinessProbe.HTTPGet.Port.String())
	service := &corev1.Service{}
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(findByKind(objs, "Service").Object, service))
	require.Len(t, service.Spec.Ports, 1)
	assert.Equal(t, "gpu-metrics", service.Spec.Ports[0].Name)
	assert.Equal(t, int32(9400), service.Spec.Ports[0].Port)
	assert.Equal(t, intstr.FromString(dcgmexporter.MetricsPortName), service.Spec.Ports[0].TargetPort)

	require.Len(t, ctr.Resources.Claims, 1)
	assert.Equal(t, "admin-gpus", ctr.Resources.Claims[0].Name)
}

func TestDCGMExporterDisabled(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{Enabled: new(false)})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	assert.Empty(t, objs)
}

func TestDCGMExporterRemoteEngineWhenDCGMEnabled(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{})
	cr.Spec.DCGM = &nvidiav1.DCGMSpec{Enabled: new(true)}

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	env := envMap(ds.Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, env, "DCGM_REMOTE_HOSTENGINE_INFO")
	assert.Contains(t, exporterConfig(t, objs), "hostengine: nvidia-dcgm-dra:5555")
}

func TestDCGMExporterPodMetadataEnrichment(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		EnablePodLabels:        new(true),
		EnablePodUID:           new(true),
		PodLabelAllowlistRegex: []string{"^app$", "^team$"},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	podSpec := ds.Spec.Template.Spec
	env := envMap(podSpec.Containers[0].Env)
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_LABELS")
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_UID")
	config := exporterConfig(t, objs)
	assert.Contains(t, config, "pod_uid: true")
	assert.Contains(t, config, "allow:\n      - ^app$\n      - ^team$")
}

func TestDCGMExporterCustomMetricsConfig(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		MetricsConfig: &nvidiav1.DCGMExporterMetricsConfig{Name: "custom-dcgm-exporter-metrics"},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	assert.Contains(t, exporterConfig(t, objs), "file: /etc/dcgm-exporter/operator/metrics/dcgm-metrics.csv")
	mount := findVolumeMountByName(ds.Spec.Template.Spec.Containers[0].VolumeMounts, "metrics-config")
	require.NotNil(t, mount)
	assert.Equal(t, "/etc/dcgm-exporter/operator/metrics", mount.MountPath)
	assert.Empty(t, mount.SubPath)

	vol := findVolume(t, ds, "metrics-config")
	require.NotNil(t, vol.ConfigMap)
	assert.Equal(t, "custom-dcgm-exporter-metrics", vol.ConfigMap.Name)
	assert.Equal(t, []corev1.KeyToPath{{Key: "dcgm-metrics.csv", Path: "dcgm-metrics.csv"}}, vol.ConfigMap.Items)
	assert.Contains(t, ds.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name: "metrics-config", ReadOnly: true, MountPath: "/etc/dcgm-exporter/operator/metrics",
	}, "the ConfigMap key is projected into its parent directory without subPath")
}

func TestDCGMExporterCollectorOverrides(t *testing.T) {
	state := newTestDCGMExporterState(t, false)
	spec := &nvidiav1.DCGMExporterSpec{
		MetricsConfig: &nvidiav1.DCGMExporterMetricsConfig{Name: "counters"},
		Args:          []string{"--collectors=/user/flags.csv"},
		Env:           []nvidiav1.EnvVar{{Name: "DCGM_EXPORTER_COLLECTORS", Value: "/user/env.csv"}},
	}
	objects, err := state.getManifestObjects(context.Background(), exporterCR(spec), draSupportedCatalog())
	require.NoError(t, err)
	container := findDaemonSet(t, objects).Spec.Template.Spec.Containers[0]
	assert.Equal(t, spec.Args, container.Args)
	assert.Equal(t, "/user/env.csv", envMap(container.Env)["DCGM_EXPORTER_COLLECTORS"])
	assert.Contains(t, container.VolumeMounts, corev1.VolumeMount{
		Name: "metrics-config", ReadOnly: true, MountPath: "/etc/dcgm-exporter/operator/metrics",
	})
}

func TestDCGMExporterNativeConfigAndWebSecretProjection(t *testing.T) {
	webSelector := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "exporter-web-v1"},
		Key:                  "web-config.yaml",
	}
	spec := &nvidiav1.DCGMExporterSpec{
		NativeConfig: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "exporter-config-v1"},
			Key:                  "custom-exporter.yaml",
		},
		WebConfig: webSelector,
	}
	configBytes, err := dcgmexporter.RenderConfig(dcgmexporter.ConfigOptionsFromSpec(spec, "", false, true))
	require.NoError(t, err)
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: spec.NativeConfig.Name, Namespace: "test-operator"},
		Immutable:  &immutable,
		Data:       map[string]string{spec.NativeConfig.Key: string(configBytes)},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: webSelector.Name, Namespace: "test-operator"},
		Data: map[string][]byte{
			webSelector.Key: []byte("tls config"),
			"tls.crt":       []byte("certificate"),
			"tls.key":       []byte("private key"),
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap, secret).Build()
	s := newTestDCGMExporterState(t, false)
	s.client = k8sClient

	objs, err := s.getManifestObjects(context.Background(), exporterCR(spec), draSupportedCatalog())
	require.NoError(t, err)
	daemonSet := findDaemonSet(t, objs)
	podSpec := daemonSet.Spec.Template.Spec
	configVolume := findVolume(t, daemonSet, dcgmexporter.ConfigVolumeName)
	require.NotNil(t, configVolume.ConfigMap)
	assert.Equal(t, spec.NativeConfig.Name, configVolume.ConfigMap.Name)
	require.Len(t, configVolume.ConfigMap.Items, 1)
	assert.Equal(t, spec.NativeConfig.Key, configVolume.ConfigMap.Items[0].Key)
	assert.Equal(t, dcgmexporter.ConfigFileName, configVolume.ConfigMap.Items[0].Path)
	assert.Equal(t, dcgmexporter.ConfigFilePath, envMap(podSpec.Containers[0].Env)[dcgmexporter.ConfigFileEnvName])
	assert.NotContains(t, daemonSet.Spec.Template.Annotations, dcgmexporter.ConfigAnnotation)

	webVolume := findVolume(t, daemonSet, dcgmexporter.WebConfigVolumeName)
	require.NotNil(t, webVolume.Secret)
	assert.Equal(t, webSelector.Name, webVolume.Secret.SecretName)
	require.NotNil(t, webVolume.Secret.DefaultMode)
	assert.Equal(t, int32(0440), *webVolume.Secret.DefaultMode)
	assert.Nil(t, webVolume.Secret.Items, "all Secret keys are projected so certificate files remain available")
	require.Contains(t, podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      dcgmexporter.WebConfigVolumeName,
		ReadOnly:  true,
		MountPath: dcgmexporter.WebConfigDirectory,
	})

	assert.Contains(t, string(configBytes), dcgmexporter.WebConfigFilePathForKey(webSelector.Key))
	assert.Equal(t, 0, kindCounts(objs)["ConfigMap"], "native configuration must remain in the selected immutable ConfigMap")
}

func TestDCGMExporterInvalidNativeConfigKeepsPreviousDaemonSet(t *testing.T) {
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		NativeConfig: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "mutable-config"},
			Key:                  "exporter.yaml",
		},
	})
	previous := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter-dra", Namespace: "test-operator"},
		Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "existing-exporter"}}}}},
	}
	immutable := false
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "mutable-config", Namespace: "test-operator"},
		Immutable:  &immutable,
		Data:       map[string]string{"exporter.yaml": "not a complete native revision"},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(restMapperWithServiceMonitor(false)).
		WithObjects(previous, configMap).
		Build()
	s := newTestDCGMExporterState(t, false)
	s.client = k8sClient

	state, err := s.Sync(context.Background(), cr, draSupportedCatalog())
	require.ErrorContains(t, err, "must be immutable")
	assert.Equal(t, SyncState(SyncStateNotReady), state)

	retained := &appsv1.DaemonSet{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(previous), retained))
	assert.Equal(t, previous, retained)
}

func TestDCGMExporterHPCJobMapping(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		HPCJobMapping: &nvidiav1.DCGMExporterHPCJobMappingConfig{
			Enabled:   new(true),
			Directory: "/run/nvidia/dcgm-job-mapping",
		},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	assert.Contains(t, exporterConfig(t, objs), "mapping_dir: /run/nvidia/dcgm-job-mapping")
	assert.NotContains(t, envMap(ds.Spec.Template.Spec.Containers[0].Env), "DCGM_HPC_JOB_MAPPING_DIR")
	vol := findVolume(t, ds, "hpc-job-mapping")
	require.NotNil(t, vol.HostPath)
	assert.Equal(t, "/run/nvidia/dcgm-job-mapping", vol.HostPath.Path)
}

func TestDCGMExporterConfigAnnotationExcludesMetricsConfigMapSelection(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	firstCR := exporterCR(&nvidiav1.DCGMExporterSpec{
		MetricsConfig: &nvidiav1.DCGMExporterMetricsConfig{Name: "csv-one"},
	})
	secondCR := exporterCR(&nvidiav1.DCGMExporterSpec{
		MetricsConfig: &nvidiav1.DCGMExporterMetricsConfig{Name: "csv-two"},
	})

	firstObjects, err := s.getManifestObjects(context.Background(), firstCR, draSupportedCatalog())
	require.NoError(t, err)
	secondObjects, err := s.getManifestObjects(context.Background(), secondCR, draSupportedCatalog())
	require.NoError(t, err)
	firstDS := findDaemonSet(t, firstObjects)
	secondDS := findDaemonSet(t, secondObjects)
	assert.Equal(t,
		firstDS.Spec.Template.Annotations[dcgmexporter.ConfigAnnotation],
		secondDS.Spec.Template.Annotations[dcgmexporter.ConfigAnnotation],
		"changing the selected CSV ConfigMap does not change generated config YAML")
	assert.Equal(t, exporterConfig(t, firstObjects), exporterConfig(t, secondObjects))
	assert.NotEqual(t,
		findVolume(t, firstDS, "metrics-config").ConfigMap.Name,
		findVolume(t, secondDS, "metrics-config").ConfigMap.Name)
}

func TestDCGMExporterGeneratedConfigAnnotationDrivesRollout(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	render := func(spec *nvidiav1.DCGMExporterSpec) string {
		objs, err := s.getManifestObjects(context.Background(), exporterCR(spec), draSupportedCatalog())
		require.NoError(t, err)
		ds := findDaemonSet(t, objs)
		return ds.Spec.Template.Annotations[dcgmexporter.ConfigAnnotation]
	}

	first := render(&nvidiav1.DCGMExporterSpec{})
	same := render(&nvidiav1.DCGMExporterSpec{})
	changed := render(&nvidiav1.DCGMExporterSpec{EnablePodLabels: new(true)})
	assert.Equal(t, first, same, "identical generated YAML keeps the Pod template stable")
	assert.NotEqual(t, first, changed, "a config change changes the Pod template annotation and triggers rollout")
}

func TestDCGMExporterConfigMapNameCollisionSurvivesEnabledAndDisabledModes(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			s := newTestDCGMExporterState(t, false)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			userConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter-config", Namespace: "test-operator", Labels: map[string]string{"owner": "user"}},
				Data:       map[string]string{"user-data": "preserve-me"},
			}
			s.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(userConfigMap.DeepCopy()).Build()

			spec := &nvidiav1.DCGMExporterSpec{Enabled: &enabled}
			if enabled {
				spec.MetricsConfig = &nvidiav1.DCGMExporterMetricsConfig{Name: userConfigMap.Name}
			}
			objs, err := s.getManifestObjects(context.Background(), exporterCR(spec), draSupportedCatalog())
			require.NoError(t, err)
			if enabled {
				assert.Equal(t, 0, kindCounts(objs)["ConfigMap"])
				ds := findDaemonSet(t, objs)
				metricsVolume := findVolume(t, ds, "metrics-config")
				require.NotNil(t, metricsVolume.ConfigMap)
				assert.Equal(t, userConfigMap.Name, metricsVolume.ConfigMap.Name)
				assert.NotNil(t, findVolume(t, ds, dcgmexporter.ConfigVolumeName).DownwardAPI)
			} else {
				assert.Empty(t, objs)
			}

			actual := &corev1.ConfigMap{}
			require.NoError(t, s.client.Get(context.Background(), types.NamespacedName{Namespace: userConfigMap.Namespace, Name: userConfigMap.Name}, actual))
			assert.Equal(t, userConfigMap.Labels, actual.Labels)
			assert.Equal(t, userConfigMap.Data, actual.Data)
		})
	}
}

func TestDCGMExporterRejectsReservedOrOversizedPodAnnotations(t *testing.T) {
	for _, test := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{name: "reserved generated config key", annotations: map[string]string{dcgmexporter.ConfigAnnotation: "user-value"}, want: "reserved"},
		{name: "annotation size limit", annotations: map[string]string{"user.example/config": strings.Repeat("x", 300*1024)}, want: "reduce exporter settings or other Pod annotations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestDCGMExporterState(t, false)
			cr := exporterCR(&nvidiav1.DCGMExporterSpec{Annotations: test.annotations})
			_, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "version: 2")
		})
	}
}

func TestDCGMExporterKubernetesExpandedAddressPassesThrough(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{Args: []string{"--address=$(EXPORTER_LISTEN_ADDRESS)"}})
	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	ds := findDaemonSet(t, objs)
	assert.Equal(t, []string{"--address=$(EXPORTER_LISTEN_ADDRESS)"}, ds.Spec.Template.Spec.Containers[0].Args)
}

func TestDCGMExporterLoopbackOverridesUseLoopbackHealthProbes(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		Env: []nvidiav1.EnvVar{{Name: "DCGM_REMOTE_HOSTENGINE_INFO", Value: "localhost:5555"}},
	})
	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	ds := findDaemonSet(t, objs)
	assert.True(t, ds.Spec.Template.Spec.HostNetwork)
	assert.Equal(t, corev1.DNSClusterFirstWithHostNet, ds.Spec.Template.Spec.DNSPolicy)
	assert.Equal(t, "127.0.0.1", ds.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Host)
	healthPort, err := dcgmexporter.DefaultHealthPort()
	require.NoError(t, err)
	assert.Contains(t, exporterConfig(t, objs), fmt.Sprintf("listen_health: 127.0.0.1:%d", healthPort))
}

func TestDCGMExporterRejectsManagedConfigOverrides(t *testing.T) {
	metricsPort, err := dcgmexporter.DefaultMetricsPort()
	require.NoError(t, err)
	for _, test := range []struct {
		name string
		spec *nvidiav1.DCGMExporterSpec
	}{
		{name: "config selector env", spec: &nvidiav1.DCGMExporterSpec{Env: []nvidiav1.EnvVar{{Name: dcgmexporter.ConfigFileEnvName}}}},
		{name: "listener conflict", spec: &nvidiav1.DCGMExporterSpec{Args: []string{"--address", fmt.Sprintf("127.0.0.1:%d", metricsPort)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestDCGMExporterState(t, false)
			_, err := s.getManifestObjects(context.Background(), exporterCR(test.spec), draSupportedCatalog())
			require.Error(t, err)
		})
	}
}

func exporterConfig(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	daemonSet := findByKind(objs, "DaemonSet")
	require.NotNil(t, daemonSet)
	config, found, err := unstructured.NestedString(daemonSet.Object, "spec", "template", "metadata", "annotations", dcgmexporter.ConfigAnnotation)
	require.NoError(t, err)
	require.True(t, found)
	return config
}

func TestDCGMExporterServiceMonitorSkippedWithoutCRD(t *testing.T) {
	s := newTestDCGMExporterState(t, false) // ServiceMonitor CRD not served
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.DCGMExporterServiceMonitorConfig{Enabled: new(true)},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	assert.Equal(t, 0, kindCounts(objs)["ServiceMonitor"],
		"ServiceMonitor must be skipped when the Prometheus Operator CRD is absent")
}

func TestDCGMExporterServiceMonitorRendered(t *testing.T) {
	s := newTestDCGMExporterState(t, true) // ServiceMonitor CRD served
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.DCGMExporterServiceMonitorConfig{
			Enabled:  new(true),
			Interval: "15s",
		},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	sm := findByKind(objs, "ServiceMonitor")
	require.NotNil(t, sm, "ServiceMonitor must render when the CRD is served and it is enabled")
	assert.Equal(t, "nvidia-dcgm-exporter-dra", sm.GetName())
}

func TestDCGMExporterServiceMonitorBasicAuthUsesHTTPS(t *testing.T) {
	s := newTestDCGMExporterState(t, true)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.DCGMExporterServiceMonitorConfig{
			Enabled: new(true),
			BasicAuth: &promv1.BasicAuth{
				Username: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "exporter-auth"},
					Key:                  "username",
				},
				Password: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "exporter-auth"},
					Key:                  "password",
				},
			},
		},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	serviceMonitor := findByKind(objs, "ServiceMonitor")
	require.NotNil(t, serviceMonitor)
	endpoints, found, err := unstructured.NestedSlice(serviceMonitor.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)
	endpoint := endpoints[0].(map[string]any)
	assert.Equal(t, "HTTPS", endpoint["scheme"])
	assert.NotContains(t, endpoint, "tlsConfig")
	assert.Contains(t, endpoint, "basicAuth")
}

func TestDCGMExporterServiceMonitorRelabelings(t *testing.T) {
	s := newTestDCGMExporterState(t, true)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.DCGMExporterServiceMonitorConfig{
			Enabled: new(true),
			TLSConfig: &promv1.SafeTLSConfig{
				CA: promv1.SecretOrConfigMap{Secret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "prometheus-ca"},
					Key:                  "ca.crt",
				}},
				ServerName: new("dcgm-exporter.example"),
			},
			BasicAuth: &promv1.BasicAuth{
				Username: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "dcgm-exporter-auth"},
					Key:                  "username",
				},
				Password: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "dcgm-exporter-auth"},
					Key:                  "password",
				},
			},
			Relabelings: []*promv1.RelabelConfig{{
				Action:       "replace",
				Regex:        "(.*)",
				SourceLabels: []promv1.LabelName{"__meta_kubernetes_pod_node_name"},
				TargetLabel:  "instance",
			}},
			MetricRelabelings: []*promv1.RelabelConfig{{
				Action:       "replace",
				Regex:        "(.+)",
				SourceLabels: []promv1.LabelName{"exported_namespace"},
				TargetLabel:  "namespace",
			}},
		},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	sm := findByKind(objs, "ServiceMonitor")
	require.NotNil(t, sm)

	endpoints, _, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.Len(t, endpoints, 1)
	endpoint := endpoints[0].(map[string]any)

	assert.Equal(t, "HTTPS", endpoint["scheme"])
	tlsConfig, ok := endpoint["tlsConfig"].(map[string]any)
	require.True(t, ok)
	ca, ok := tlsConfig["ca"].(map[string]any)
	require.True(t, ok)
	secret, ok := ca["secret"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "prometheus-ca", secret["name"])
	assert.Equal(t, "ca.crt", secret["key"])
	auth, ok := endpoint["basicAuth"].(map[string]any)
	require.True(t, ok)
	username, ok := auth["username"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "dcgm-exporter-auth", username["name"])
	assert.Equal(t, "username", username["key"])

	relabelings, ok := endpoint["relabelings"].([]any)
	require.True(t, ok, "relabelings must be rendered")
	require.Len(t, relabelings, 1)
	assert.Equal(t, "instance", relabelings[0].(map[string]any)["targetLabel"])

	metricRelabelings, ok := endpoint["metricRelabelings"].([]any)
	require.True(t, ok, "metricRelabelings must be rendered")
	require.Len(t, metricRelabelings, 1)
	assert.Equal(t, "namespace", metricRelabelings[0].(map[string]any)["targetLabel"])
}

func TestDCGMExporterServiceType(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceSpec: &nvidiav1.DCGMExporterServiceConfig{
			Type:                  corev1.ServiceTypeNodePort,
			InternalTrafficPolicy: ptr.To(corev1.ServiceInternalTrafficPolicyLocal),
		},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	svc := findByKind(objs, "Service")
	require.NotNil(t, svc)
	svcType, _, _ := unstructured.NestedString(svc.Object, "spec", "type")
	assert.Equal(t, "NodePort", svcType)
	itpValue, _, _ := unstructured.NestedString(svc.Object, "spec", "internalTrafficPolicy")
	assert.Equal(t, "Local", itpValue)
}
