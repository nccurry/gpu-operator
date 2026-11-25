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
	"regexp"
	"strconv"
	"strings"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

const (
	metricsListenEnvName     = "DCGM_EXPORTER_LISTEN"
	metricsCollectorsEnvName = "DCGM_EXPORTER_COLLECTORS"
)

var kubernetesEnvExpansion = regexp.MustCompile(`\$\([A-Za-z_][A-Za-z0-9_]*\)`)

// ValidateConfigFileSelection rejects user settings that replace the
// Operator-generated v2 configuration file.
func ValidateConfigFileSelection(args []string, env []gpuv1.EnvVar) error {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--config-file" || arg == "-config-file" ||
			strings.HasPrefix(arg, "--config-file=") || strings.HasPrefix(arg, "-config-file=") {
			return fmt.Errorf("dcgmExporter.args selects a separate config file with %s; remove it and move supported settings to GPU Operator fields", arg)
		}
	}
	for _, variable := range env {
		if variable.Name == ConfigFileEnvName {
			return fmt.Errorf("dcgmExporter.env sets %s; remove it and move supported settings to GPU Operator fields", ConfigFileEnvName)
		}
	}
	return nil
}

// ValidateUserOverrides accepts settings that keep the Operator-managed
// Service, config file, and custom metrics mount usable.
func ValidateUserOverrides(args []string, env []gpuv1.EnvVar) error {
	if err := ValidateConfigFileSelection(args, env); err != nil {
		return err
	}
	metricsPort, err := DefaultMetricsPort()
	if err != nil {
		return err
	}
	if value, source, explicit := effectiveStringOption(args, env, metricsListenEnvName, "--address", "-address", "--a", "-a"); explicit && !serviceAddressOverride(value, strings.HasPrefix(source, "-"), metricsPort) {
		return fmt.Errorf("%s sets metrics listener %q, which does not match the Operator-managed container port %d; keep a wildcard listener on port %d", source, value, metricsPort, metricsPort)
	}
	if value, source, explicit := effectiveStringOption(args, env, metricsCollectorsEnvName, "--collectors", "-collectors", "--f", "-f"); explicit && value == oldMetricsFilePath {
		return fmt.Errorf("%s selects the former custom metrics path %q; use %q", source, oldMetricsFilePath, CustomMetricsFilePath)
	}
	return nil
}

func effectiveStringOption(args []string, env []gpuv1.EnvVar, envName string, flags ...string) (string, string, bool) {
	value, source, explicit := "", "", false
	for _, variable := range env {
		if variable.Name == envName {
			value, source, explicit = variable.Value, variable.Name, true
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		for _, flag := range flags {
			if arg == flag {
				value, source, explicit = "", arg, true
				if i+1 < len(args) && args[i+1] != "--" {
					i++
					value = args[i]
				}
				break
			}
			if strings.HasPrefix(arg, flag+"=") {
				_, value, _ = strings.Cut(arg, "=")
				source, explicit = arg, true
				break
			}
		}
	}
	return value, source, explicit
}

func effectiveBoolOption(defaultValue bool, args []string, env []gpuv1.EnvVar, envName string, flags ...string) bool {
	value := defaultValue
	for _, variable := range env {
		if variable.Name == envName {
			if parsed, err := strconv.ParseBool(variable.Value); err == nil {
				value = parsed
			}
		}
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		for _, flag := range flags {
			if arg == flag {
				value = true
				break
			}
			if raw, ok := strings.CutPrefix(arg, flag+"="); ok {
				if parsed, err := strconv.ParseBool(raw); err == nil {
					value = parsed
				}
				break
			}
		}
	}
	return value
}

func serviceAddress(address string, metricsPort int32) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber != int(metricsPort) {
		return false
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

func serviceAddressOverride(address string, allowKubernetesExpansion bool, metricsPort int32) bool {
	if !allowKubernetesExpansion || !kubernetesEnvExpansion.MatchString(address) {
		return serviceAddress(address, metricsPort)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		matches := kubernetesEnvExpansion.FindAllStringIndex(address, -1)
		return len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(address)
	}

	hostAllowed := host == "" || host == "0.0.0.0" || host == "::" || kubernetesEnvExpansion.MatchString(host)
	if !hostAllowed {
		return false
	}
	if kubernetesEnvExpansion.MatchString(port) {
		return true
	}
	portNumber, err := strconv.Atoi(port)
	return err == nil && portNumber == int(metricsPort)
}
