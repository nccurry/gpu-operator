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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
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
	assert.Equal(t, "nvcr.io/nvidia/k8s/dcgm-exporter:4.5.3", ctr.Image)
	env := envMap(ctr.Env)
	assert.Equal(t, ":9400", env["DCGM_EXPORTER_LISTEN"])
	// Pod attribution on DRA nodes is on by default; pod labels and UID are not.
	assert.Equal(t, "true", env["KUBERNETES_ENABLE_DRA"])
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_LABELS")
	assert.NotContains(t, env, "DCGM_EXPORTER_KUBERNETES_ENABLE_POD_UID")
	assert.Equal(t, "/etc/dcgm-exporter/default-counters.csv", env["DCGM_EXPORTER_COLLECTORS"])
	assert.Equal(t, &corev1.SecurityContext{Privileged: new(true)}, ctr.SecurityContext)
	// Embedded engine: no remote host-engine env.
	_, hasRemote := env["DCGM_REMOTE_HOSTENGINE_INFO"]
	assert.False(t, hasRemote, "exporter must run embedded when standalone DCGM is disabled")

	require.Len(t, ctr.Resources.Claims, 1)
	assert.Equal(t, "admin-gpus", ctr.Resources.Claims[0].Name)
}

func TestDCGMExporterSecurityContext(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	securityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		ReadOnlyRootFilesystem:   new(true),
		RunAsGroup:               new(int64(65532)),
		RunAsNonRoot:             new(true),
		RunAsUser:                new(int64(65532)),
	}
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{SecurityContext: securityContext})

	objects, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	assert.Equal(t, securityContext, findDaemonSet(t, objects).Spec.Template.Spec.Containers[0].SecurityContext)
}

func TestDCGMExporterSharedDaemonsetSettings(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	resources := &nvidiav1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	fsGroup := int64(2000)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{Resources: resources})
	cr.Spec.Daemonsets.PodSecurityContext = &corev1.PodSecurityContext{FSGroup: &fsGroup}
	cr.Spec.Daemonsets.PriorityClassName = "exporter-priority"
	cr.Spec.Daemonsets.Tolerations = []corev1.Toleration{{Key: "exporter-taint", Operator: corev1.TolerationOpExists}}
	cr.Spec.Daemonsets.UpdateStrategy = "OnDelete"

	objects, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	daemonset := findDaemonSet(t, objects)
	podSpec := daemonset.Spec.Template.Spec
	assert.Equal(t, appsv1.OnDeleteDaemonSetStrategyType, daemonset.Spec.UpdateStrategy.Type)
	assert.Equal(t, &corev1.PodSecurityContext{FSGroup: &fsGroup}, podSpec.SecurityContext)
	assert.Equal(t, "exporter-priority", podSpec.PriorityClassName)
	assert.Equal(t, []corev1.Toleration{{Key: "exporter-taint", Operator: corev1.TolerationOpExists}}, podSpec.Tolerations)

	container := podSpec.Containers[0]
	assert.Equal(t, resources.Limits, container.Resources.Limits)
	assert.Equal(t, resources.Requests, container.Resources.Requests)
	assert.Equal(t, &corev1.SecurityContext{Privileged: new(true)}, container.SecurityContext)
	require.Len(t, podSpec.ResourceClaims, 1)
	assert.Equal(t, "admin-gpus", podSpec.ResourceClaims[0].Name)
	assert.Equal(t, "true", podSpec.NodeSelector["nvidia.com/gpu.deploy.dcgm-exporter-dra"])
}

func TestDCGMExporterRollingUpdateSettings(t *testing.T) {
	testCases := map[string]struct {
		maxUnavailable string
		wantError      bool
		wantType       intstr.Type
		wantInt        int32
		wantString     string
	}{
		"default": {},
		"number":  {maxUnavailable: "3", wantType: intstr.Int, wantInt: 3},
		"percent": {maxUnavailable: "25%", wantType: intstr.String, wantString: "25%"},
		"invalid": {maxUnavailable: "10%abc", wantError: true},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			s := newTestDCGMExporterState(t, false)
			cr := exporterCR(&nvidiav1.DCGMExporterSpec{})
			if tc.maxUnavailable != "" {
				cr.Spec.Daemonsets.RollingUpdate = &nvidiav1.RollingUpdateSpec{MaxUnavailable: tc.maxUnavailable}
			}

			objects, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
			if tc.wantError {
				require.ErrorContains(t, err, "failed to apply rolling update config")
				return
			}
			require.NoError(t, err)

			strategy := findDaemonSet(t, objects).Spec.UpdateStrategy
			assert.Equal(t, appsv1.RollingUpdateDaemonSetStrategyType, strategy.Type)
			if tc.maxUnavailable == "" {
				assert.Nil(t, strategy.RollingUpdate)
				return
			}
			require.NotNil(t, strategy.RollingUpdate)
			require.NotNil(t, strategy.RollingUpdate.MaxUnavailable)
			assert.Equal(t, tc.wantType, strategy.RollingUpdate.MaxUnavailable.Type)
			assert.Equal(t, tc.wantInt, strategy.RollingUpdate.MaxUnavailable.IntVal)
			assert.Equal(t, tc.wantString, strategy.RollingUpdate.MaxUnavailable.StrVal)
		})
	}
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
	assert.Equal(t, "nvidia-dcgm-dra:5555", env["DCGM_REMOTE_HOSTENGINE_INFO"])
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
	assert.Equal(t, "true", env["DCGM_EXPORTER_KUBERNETES_ENABLE_POD_LABELS"])
	assert.Equal(t, "true", env["DCGM_EXPORTER_KUBERNETES_ENABLE_POD_UID"])
	assert.Equal(t, "^app$,^team$", env["DCGM_EXPORTER_KUBERNETES_POD_LABEL_ALLOWLIST_REGEX"])
}

func TestDCGMExporterCustomMetricsConfig(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		MetricsConfig: &nvidiav1.DCGMExporterMetricsConfig{Name: "custom-dcgm-exporter-metrics"},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	env := envMap(ds.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "/etc/dcgm-exporter/dcgm-metrics.csv", env["DCGM_EXPORTER_COLLECTORS"])

	vol := findVolume(t, ds, "metrics-config")
	require.NotNil(t, vol.ConfigMap)
	assert.Equal(t, "custom-dcgm-exporter-metrics", vol.ConfigMap.Name)
}

func TestDCGMExporterHPCJobMapping(t *testing.T) {
	s := newTestDCGMExporterState(t, false)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		HPCJobMapping: &nvidiav1.DCGMExporterHPCJobMappingConfig{Enabled: new(true)},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)

	ds := findDaemonSet(t, objs)
	env := envMap(ds.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "/var/lib/dcgm-exporter/job-mapping", env["DCGM_HPC_JOB_MAPPING_DIR"])
	vol := findVolume(t, ds, "hpc-job-mapping")
	require.NotNil(t, vol.HostPath)
	assert.Equal(t, "/var/lib/dcgm-exporter/job-mapping", vol.HostPath.Path)
}

func TestDCGMExporterServiceMonitorSkippedWithoutCRD(t *testing.T) {
	s := newTestDCGMExporterState(t, false) // ServiceMonitor CRD not served
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.ServiceMonitorConfig{Enabled: new(true)},
	})

	objs, err := s.getManifestObjects(context.Background(), cr, draSupportedCatalog())
	require.NoError(t, err)
	assert.Equal(t, 0, kindCounts(objs)["ServiceMonitor"],
		"ServiceMonitor must be skipped when the Prometheus Operator CRD is absent")
}

func TestDCGMExporterServiceMonitorRendered(t *testing.T) {
	s := newTestDCGMExporterState(t, true) // ServiceMonitor CRD served
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.ServiceMonitorConfig{
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

func TestDCGMExporterServiceMonitorRelabelings(t *testing.T) {
	s := newTestDCGMExporterState(t, true)
	cr := exporterCR(&nvidiav1.DCGMExporterSpec{
		ServiceMonitor: &nvidiav1.ServiceMonitorConfig{
			Enabled: new(true),
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
