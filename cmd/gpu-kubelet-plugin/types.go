/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

const (
	GpuDeviceType = "gpu"
	// Allocatable MIG device which is managed out-of-band. Note(JP): I think
	// this is exposed as part of the API, and was so far always "mig",
	// referring to the "static MIG" use case. Upon introducing "migdyn" below,
	// I felt inclined to rename this to "migstatic", but I think we cannot do
	// that.
	MigStaticDeviceType = "mig"
	// Allocatable MIG device which is manged by us.
	MigDynamicDeviceType = "migdyn"
	VfioDeviceType       = "vfio"
	UnknownDeviceType    = "unknown"
)

type UUIDProvider interface {
	// Both, full GPUs and MIG devices
	UUIDs() []string
	// Only full GPUs
	GpuUUIDs() []string
	// Only MIG devices
	MigDeviceUUIDs() []string
}

func ResourceClaimToString(rc *resourcev1.ResourceClaim) string {
	return fmt.Sprintf("%s/%s:%s", rc.Namespace, rc.Name, rc.UID)
}

func ClaimsToStrings(claims []*resourcev1.ResourceClaim) []string {
	var results []string
	for _, c := range claims {
		results = append(results, ResourceClaimToString(c))
	}
	return results
}

func ClaimRefsToStrings(claimRefs []kubeletplugin.NamespacedObject) []string {
	var results []string
	for _, r := range claimRefs {
		results = append(results, r.String())
	}
	return results
}
