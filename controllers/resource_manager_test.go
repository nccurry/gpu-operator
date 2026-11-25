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

package controllers

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

func TestManifestKind(t *testing.T) {
	tests := []struct {
		name         string
		manifest     string
		expectedKind string
		errorMessage string
	}{
		{
			name: "YAML",
			manifest: `apiVersion: v1
kind: ServiceAccount
metadata:
  name: test
`,
			expectedKind: "ServiceAccount",
		},
		{
			name:         "JSON",
			manifest:     `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test"}}`,
			expectedKind: "ConfigMap",
		},
		{
			name: "kind-like text in another field",
			manifest: `apiVersion: v1
metadata:
  annotations:
    example.com/value: "kind: WrongKind"
  name: test
kind: Service
`,
			expectedKind: "Service",
		},
		{
			name: "missing kind",
			manifest: `apiVersion: v1
metadata:
  name: test
`,
			errorMessage: "manifest is missing kind",
		},
		{
			name:         "malformed YAML",
			manifest:     "kind: [",
			errorMessage: "failed to decode manifest metadata",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, err := manifestKind([]byte(tc.manifest))
			if tc.errorMessage != "" {
				require.ErrorContains(t, err, tc.errorMessage)
				require.Empty(t, kind)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedKind, kind)
		})
	}
}

func TestAllAssetsHaveManifestKind(t *testing.T) {
	root, err := os.OpenRoot("../assets")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, root.Close())
	})

	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		if entry.IsDir() {
			return nil
		}

		manifest, err := root.ReadFile(path)
		require.NoError(t, err)
		kind, err := manifestKind(manifest)
		require.NoErrorf(t, err, "failed to read kind from %s", path)
		require.NotEmptyf(t, kind, "empty kind in %s", path)
		return nil
	})
	require.NoError(t, err)
}

func TestDCGMExporterDoesNotManageLegacyConfigMapName(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			userConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter-config", Namespace: "gpu-operator", Labels: map[string]string{"owner": "user"}},
				Data:       map[string]string{"user-data": "preserve-me"},
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(userConfigMap.DeepCopy()).Build()
			controller := &ClusterPolicyController{
				client: k8sClient,
				ctx:    context.Background(),
				logger: ctrl.Log.WithName("test"),
				singleton: &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
					DCGMExporter: gpuv1.DCGMExporterSpec{Enabled: &enabled},
				}},
			}

			resources, _ := addResourcesControls(controller, filepath.Join("..", dcgmExporterAssetsPath))
			require.Empty(t, resources.ConfigMaps)
			controller.resources = []Resources{resources}
			controller.stateNames = []string{"state-dcgm-exporter"}
			controller.idx = 0
			_, err := ConfigMaps(*controller)
			require.NoError(t, err)

			actual := &corev1.ConfigMap{}
			require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: userConfigMap.Namespace, Name: userConfigMap.Name}, actual))
			require.Equal(t, userConfigMap.Labels, actual.Labels)
			require.Equal(t, userConfigMap.Data, actual.Data)
		})
	}
}
