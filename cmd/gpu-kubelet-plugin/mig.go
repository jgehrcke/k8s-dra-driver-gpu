/*
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
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
	"regexp"
	"strings"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

// The following 3-tuple precisely describes a physical MIG device
// configuration, before or after creation: parent (UUID or minor),
// Placement.Start, and ProfileID. The profile ID implies slice count.
type MigTuple struct {
	ParentMinor GPUMinor
	// What's commonly called the "MIG profile ID" here, at this level of
	// detail, is actually the `GIProfileID` (GPU Instance Profile ID): This
	// corresponds to the resource slice configuration (e.g., 1g.5gb). It
	// defines the slice count and memory size. This is the ID one passes into
	// nvmlDeviceCreateGpuInstance() to create the partition.
	ProfileID      int
	PlacementStart int
}

// After creation, a specific MIG device can also be fully identified by the
// following 3-tuple: the parent GPU (UUID/minor), the GPU Instance (GI)
// identifier, and the Compute Instance (CI) identifier. But that counts only
// for as long as the device is known to be alive. This can be checked by
// confirming actual vs. expected MIG device UUID after looking up the device by
// (parent, CIID, GIID). TODO: maybe encode MIG UUID here as that 'expected'
// UUID?
type MigTupleLogical struct {
	ParentMinor GPUMinor
	GIID        int
	CIID        int
}

// MigSpec describes an abstract MIG device with precise specification of parent
// GPU, MIG profile and physcical placement on the parent GPU. Compared to the
// minimal tuples above, the properties of this type are more complex objects.
//
// Does not necessarily encode a _concrete_ (materialized / created /
// incarnated) device. In terms of abstract MIG device descriptions, there are
// many useful levels of precision / abstraction / refinement. For example:
//
// 1) specify MIG profile; do not specify parent, placement 2) specify MIG
// profile, parent; do not specify placement 3) specify MIG profile, parent,
// placement.
//
// This type is case (3); it leaves no degrees of freedom.
//
// TODO2(JP): clarify how CI id and GI id are not orthogonal to placement. Maybe
// add a method that translates between the two. I don't think we need to wait
// for creation to then know the CI ID and GI ID. TODO3(JP): clarify the
// relevance of the three-tuple of parent,ciid,giid for precisely describing a
// MIG device, and how that's a fundamental data structure. Maybe define its own
// type for it, and use it elsewhere.

type MigSpec struct {
	Parent        *GpuInfo
	Profile       nvdev.MigProfile
	GIProfileInfo nvml.GpuInstanceProfileInfo
	Placement     nvml.GpuInstancePlacement
}

func (m *MigSpec) Tuple() *MigTuple {
	klog.Infof("migspec tuple() with profile ID: %d", m.GIProfileInfo.Id)
	return &MigTuple{
		ParentMinor:    m.Parent.minor,
		ProfileID:      int(m.GIProfileInfo.Id),
		PlacementStart: int(m.Placement.Start),
	}
}

func (m *MigSpec) CanonicalName() DeviceName {
	return migppCanonicalName(m.Tuple(), m.Profile.String())
}

type MigProfileInfo struct {
	profile    nvdev.MigProfile
	placements []*MigDevicePlacement
}

func (p MigProfileInfo) String() string {
	return p.profile.String()
}

type MigDevicePlacement struct {
	nvml.GpuInstancePlacement
}

// Update Jan 2026: one only needs parent UUID (or minor), Placement.Start, and
// ProfileID for a complete description of the physical configuration. The
// Profile ID implies slice count.  The tuple (ParentMinor, ProfileID,
// Placement.Start) is sufficient to uniquely identify a physically instantiated
// MIG device. For recovery logic, this means when iterating through existing
// devices to find a match, we only need to check: does the device's Profile ID
// match? Does the device's Placement.Start match? If both are true, that is
// the device we're looking for.
//
// `profile string` must be what's returned by profile.String(), the
// classical/canonical profile string notation, with a dot. This must not crash
// when fed with data from a MigDeviceInfo object deserialized from JSON.
func migppCanonicalName(mt *MigTuple, profileName string) string {
	// `profileName` is for exampole `4g.95gb` -- DRA device names must not
	// contain dots, and this is used in a device name. The outer
	// `toRFC1123Compliant()` call is just to be safe; I don't see a clear need
	// for it given profileNames that I have seen.
	pname := toRFC1123Compliant(strings.ReplaceAll(profileName, ".", ""))
	return fmt.Sprintf("gpu-%d-mig-%s-%d-%d", mt.ParentMinor, pname, mt.ProfileID, mt.PlacementStart)
}

// toRFC1123Compliant converts the input to a DNS name compliant with RFC 1123.
// Note that a device name in DRA must not contain dots either (this function
// does not always return a string that can be used as a device name).
func toRFC1123Compliant(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9-.]`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	name = strings.TrimSuffix(name, ".")

	// Can this ever hurt? Should we error out?
	if len(name) > 253 {
		name = name[:253]
	}

	return name
}
