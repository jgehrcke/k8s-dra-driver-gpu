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
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// A specific MIG device (incarnated or not) is fully identified by three pieces
// of information: the parent GPU UUID, the GPU Instance (GI) identifier, and
// the Compute Instance (CI) identifier.

// MigSpec describes an abstract MIG device with precise specification of parent
// GPU, MIG profile and physcical placement on the parent GPU.
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
	// TODO(JP): rename to Placement?
	MemorySlices nvml.GpuInstancePlacement
}

func (m *MigSpec) CanonicalName() DeviceName {
	return migppCanonicalName(m.Parent.minor, m.Profile.String(), &m.MemorySlices)
}
