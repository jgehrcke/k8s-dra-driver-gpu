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
	"strconv"
	"strings"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// MigSpecTuple is a 3-tuple precisely describing a physical MIG device
// configuration: parent (defined by UUID or minor), placement start index, and
// (GI) MIG profile ID. The profile ID implies memory slice count. This
// representation is precise and meaningful before or after creation of a
// specific MIG device configuration. It does not carry MIG device identity
// (UUID).
type MigSpecTuple struct {
	ParentMinor GPUMinor
	// What is commonly called "MIG profile ID" typically refers to the GPU
	// Instance Profile ID. The GI profile ID is also what's emitted by
	// `nvidia-smi mig -lgip` for each profile -- it directly corresponds to a
	// specific human-readable profile string such as "1g.5gb". For programmatic
	// management, it is the better profile representation than the string. Most
	// importantly, the profile defines slice count and memory size. Another way
	// to describe GI profile ID: this is the ID that one passes into
	// nvmlDeviceCreateGpuInstance() to create the partition.
	ProfileID      int
	PlacementStart int
}

// Minimal, precise representation of a specific, created MIG device.
//
// After creation and during its lifetime, a specific MIG device can be
// identified by the following 3-tuple: the parent GPU (UUID/minor), the GPU
// Instance (GI) identifier, and the Compute Instance (CI) identifier. The
// GIID/CIID-based tracking is however only safe for as long as the very same
// device is known to be alive (otherwise those IDs may refer to a different
// device than assumed because they may be re-used for a potentially different
// physical configuration -- at least, there doesn't seem to be any guarantee
// that that is not the case). Hence, another parameter is tracked by this type:
// `uuid` -- a MIG device UUID changes across destruction/re-creation of the
// same physical configuration. The `uuid` carried by this type can therefore be
// used to distinguish actual vs. expected MIG device UUID after looking up a
// MIG device by (parent, CIID, GIID). What's expressed above, in other words:
// as far as I understand, there is no guaranteed relationship between GIID+CIID
// on the one hand and profileID+placementStart on the other hand.
type MigLiveTuple struct {
	ParentMinor GPUMinor
	GIID        int
	CIID        int
	uuid        string
}

// MigSpec is similar to `MigSpecTuple` as it also fundamentally encodes parent,
// profile, and placement. In that sense, it is abstract description of a
// specific MIG device configuration. Compared to `MigSpecTuple`, though, the
// properties of this type are richer objects for convenience.
type MigSpec struct {
	Parent        *GpuInfo
	Profile       nvdev.MigProfile
	GIProfileInfo nvml.GpuInstanceProfileInfo
	Placement     nvml.GpuInstancePlacement
}

func (m *MigSpec) Tuple() *MigSpecTuple {
	return &MigSpecTuple{
		ParentMinor:    m.Parent.minor,
		ProfileID:      int(m.GIProfileInfo.Id),
		PlacementStart: int(m.Placement.Start),
	}
}

// Turns MigSpecTuple into a canonical MIG device name. Needs additional input:
// The stringified MIG profile name (deliberately not stored on the type;
// implicitly stored via profile ID).
func (m *MigSpecTuple) ToCanonicalName(profileName string) DeviceName {
	pname := toRFC1123Compliant(strings.ReplaceAll(profileName, ".", ""))
	return fmt.Sprintf("gpu-%d-mig-%s-%d-%d", m.ParentMinor, pname, m.ProfileID, m.PlacementStart)
}

// NewMigSpecTupleFromCanonicalName parses a canonical MIG device name into a
// MigSpecTuple struct.
func NewMigSpecTupleFromCanonicalName(n DeviceName) (*MigSpecTuple, error) {
	return fromCanonicalName(n)
}

func (m *MigSpec) CanonicalName() DeviceName {
	return m.Tuple().ToCanonicalName(m.Profile.String())
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
// func migppCanonicalName(mt *MigSpecTuple, profileName string) string {
// 	// `profileName` is for exampole `4g.95gb` -- DRA device names must not
// 	// contain dots, and this is used in a device name. The outer
// 	// `toRFC1123Compliant()` call is just to be safe; I don't see a clear need
// 	// for it given profileNames that I have seen.
// 	pname := toRFC1123Compliant(strings.ReplaceAll(profileName, ".", ""))
// 	return fmt.Sprintf("gpu-%d-mig-%s-%d-%d", mt.ParentMinor, pname, mt.ProfileID, mt.PlacementStart)
// }

// Capture 4 groups:
// 1. ParentMinor (digits)
// 2. Profile name (greedy match of anything in the middle)
// 3. ProfileID (digits)
// 4. PlacementStart (digits)
// Anchors ^ and $ ensure that the exact, full string must match.
var canonicalNameRegex = regexp.MustCompile(`^gpu-(\d+)-mig-(.+)-(\d+)-(\d+)$`)

func fromCanonicalName(name DeviceName) (*MigSpecTuple, error) {
	matches := canonicalNameRegex.FindStringSubmatch(string(name))
	if matches == nil {
		return nil, fmt.Errorf("malformed device name: %q", name)
	}

	// matches[0]: the whole string.
	// matches[1]: = ParentMinor
	// matches[2]: MIG profile name (ignore this for building the struct)
	// matches[3]: ProfileID
	// matches[4]: PlacementStart

	// The regex guarantees these groups are digits, and the expected values are
	// small, Atoi errors are not expected. Handle them anyway for correctness.
	parentMinor, err1 := strconv.Atoi(matches[1])
	profileID, err2 := strconv.Atoi(matches[3])
	placementStart, err3 := strconv.Atoi(matches[4])

	if err1 != nil || err2 != nil || err3 != nil {
		return nil, fmt.Errorf("integer parsing failed for name %q", name)
	}

	return &MigSpecTuple{
		ParentMinor:    GPUMinor(parentMinor),
		ProfileID:      profileID,
		PlacementStart: placementStart,
	}, nil
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
