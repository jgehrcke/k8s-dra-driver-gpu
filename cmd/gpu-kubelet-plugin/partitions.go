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

	"github.com/Masterminds/semver"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

type PartCapacityMap map[resourceapi.QualifiedName]resourceapi.DeviceCapacity

// KEP 4815 device announcement: return announce device attributes for this
// physical/full GPU.
func (d *GpuInfo) PartDevAttributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")
	return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(GpuDeviceType),
		},
		"uuid": {
			StringValue: &d.UUID,
		},
		"productName": {
			StringValue: &d.productName,
		},
		"brand": {
			StringValue: &d.brand,
		},
		"architecture": {
			StringValue: &d.architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.driverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.cudaDriverVersion).String()),
		},
		"pcieBusID": {
			StringValue: &d.pcieBusID,
		},
		pciBusIDAttrName: {
			StringValue: &d.pcieBusID,
		},
	}
}

// KEP 4815 device announcement: return the full (physical) device capacity for
// this devices (this also uses information from looking at all MIG profiles
// beforehand).
func (d *GpuInfo) PartCapacities() PartCapacityMap {
	return d.maxCapacities
}

// KEP 4815 device announcement: return the name for the shared counter
// representing this full device.
func (d *GpuInfo) GetSharedCounterSetName() string {
	return toRFC1123Compliant(fmt.Sprintf("%s-counter-set", d.CanonicalName()))
}

// KEP 4815 device announcement: for now, define exactly one CounterSet per full
// GPU device. Individual partitions consume from that. In that CounterSet,
// define one counter per device capacity dimension, and add one counter
// (capacity 1) per memory slice.
func (d *GpuInfo) PartSharedCounterSets() []resourceapi.CounterSet {
	return []resourceapi.CounterSet{{
		Name:     d.GetSharedCounterSetName(),
		Counters: addCountersForMemSlices(capacitiesToCounters(d.maxCapacities), 0, d.memSliceCount),
	}}
}

// KEP 4815 device announcement: define what this full GPU consumes when allocated.
// Let the full device consume everything. Goals: 1) when the full device is
// allocated, all available counters drop to zero. 2) when the smallest
// partition gets allocated, the full device cannot be allocated anymore.
func (d *GpuInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: d.GetSharedCounterSetName(),
		Counters:   addCountersForMemSlices(capacitiesToCounters(d.maxCapacities), 0, d.memSliceCount),
	}}
}

// KEP 4815 device announcement: return the 'full' device description.
func (d *GpuInfo) PartGetDevice() resourceapi.Device {
	dev := resourceapi.Device{
		Name:             d.CanonicalName(),
		Attributes:       d.PartDevAttributes(),
		Capacity:         d.PartCapacities(),
		ConsumesCounters: d.PartConsumesCounters(),
	}

	// Not available in all environments, enrich advertised device only
	// conditionally.
	if d.pcieRootAttr != nil {
		dev.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	return dev
}

// Return the full KEP 4815 representation of an abstract MIG device.
func (i *MigSpec) PartGetDevice() resourceapi.Device {
	d := resourceapi.Device{
		Name:             i.CanonicalName(),
		Attributes:       i.PartAttributes(),
		Capacity:         i.PartCapacities(),
		ConsumesCounters: i.PartConsumesCounters(),
	}
	return d
}

// Return the KEP 4818 capacities of an abstract MIG device.
//
// TODOMIG(JP): add memory slices or not?
//
// Note(JP): for now, I feel like we may want to keep capacity agnostic to
// placement. That would imply not announcing specific memory slices as part of
// capacity (specific memory slices encode placement). That makes sense to me,
// but I may of course miss something here.
//
// I have noted that in an example spec in KEP 4815 we _do_ enumerate memory
// slices in a partition's capacity. Example:
//
//   - name: gpu-0-mig-2g.10gb-0-1
//     attributes:
//     ...
//     capacity:
//     ...
//     decoders:
//     value: "1"
//     encoders:
//     value: "0"
//     ...
//     memorySlice0:
//     value: "1"
//     memorySlice1:
//     value: "1"
//     multiprocessors:
//     value: "28"
//     ...
//
// 1) There, we only announce those slices with value 1 but we do _not_ announce
// memory slices not consumed (value: 0). That's inconsistent with other
// capacity dimensions which (in the example above) are enumerated despite
// having a value of zero (e.g. `encoders` above).
//
// 2) Semantically, to me, capacity I think can (and should?) be
// placement-agnostic. I am happy to be convinced otherwise.
//
// 3) If `capacity“ is in our case always encoding the _same_ information as
// `consumesCounters` then that's duplication and feels like an API design flaw
// or API usage flow. It feels like these parts of the resource slice device
// spec serve a different meaning, and hence allow for different information
// content. Again, I may miss something.
func (i MigSpec) PartCapacities() PartCapacityMap {
	p := i.GIProfileInfo
	return PartCapacityMap{
		"multiprocessors": intcap(p.MultiprocessorCount),
		"copyEngines":     intcap(p.CopyEngineCount),
		"decoders":        intcap(p.DecoderCount),
		"encoders":        intcap(p.EncoderCount),
		"jpegEngines":     intcap(p.JpegCount),
		"ofaEngines":      intcap(p.OfaCount),
		// In the k8s world, we love announcing unit-less memory :-). However,
		// it's important to know that `memory` here must be announced with the
		// unit "Bytes". The MIG profile's MemorySizeMB` property comes straight
		// from NVML and is documented in the public API docs with "Memory size
		// in MBytes". As it says "MB" and not MiB", one could assume that unit
		// to be 10^6 Bytes. However Update: in nvml.h this type's property is
		// documented with `memorySizeMB; //!< Device memory size (in MiB)`.
		// Hence, the unit is 2^20 Bytes (1024 * 1024 Bytes).
		"memory": intcap(int64(p.MemorySizeMB * 1024 * 1024)),
	}
}

func (i MigSpec) PartAttributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			// TODO: make "mig" dynamic again? This is exposed in the public API
			StringValue: ptr.To("mig"),
		},
		"parentUUID": {
			StringValue: &i.Parent.UUID,
		},
		// TODOMIG: expose parent index?
		// "parentIndex": {
		// 	IntValue: ptr.To(int64(i.Parent.index)), // TODO: really expose?
		// },
		// TODOMIG: really expose parent minor?
		"parentMinor": {
			IntValue: ptr.To(int64(i.Parent.minor)),
		},
		"profile": {
			StringValue: ptr.To(i.Profile.String()),
		},
		"productName": {
			StringValue: &i.Parent.productName,
		},
		"brand": {
			StringValue: &i.Parent.brand,
		},
		"architecture": {
			StringValue: &i.Parent.architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(i.Parent.cudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(i.Parent.driverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(i.Parent.cudaDriverVersion).String()),
		},
		"pcieBusID": {
			StringValue: &i.Parent.pcieBusID,
		},
	}
}

func capacitiesToCounters(m PartCapacityMap) map[string]resourceapi.Counter {
	counters := make(map[string]resourceapi.Counter)
	for name, cap := range m {
		// Automatically derive counter name from capacity to ensure consistency.
		counters[toRFC1123Compliant(string(name))] = resourceapi.Counter{Value: cap.Value}
	}
	return counters
}

// Construct and return the KEP 4815 DeviceCounterConsumption representation of
// an abstract MIG device.
//
// This device is a partition of a physical GPU. The returned
// `DeviceCounterConsumption` object describes which aspects precisely this
// partition consumes of the the full device.
//
// Each entry in capacity is modeled as a counter (consuming from the parent
// device). In addition, this MIG device, if allocated, consumes at least one
// specific memory slice. Each memory slice is modeled with its own counter
// (capacity: 1). Note that for example on a B200 GPU, the `3g.90gb` device
// consumes 4 out of 8 memory slices in total, but only 3 out of seven SMs. That
// is, with two `3g.90gb` devices allocated all memory slices are consumed, and
// one SM -- while unallocated -- cannot be used anymore. The parent is a full
// GPU device.
//
// When this device is allocated, it consumes from the parent's CounterSet. The
// parent's counter set is referred to by name. Use a naming convention:
// currently, a full GPU has precisely one counter set associated with it, and
// its name has the form 'gpu-%d-counter-set' where the placeholder is the GPU
// index (change to UUID)?
func (i MigSpec) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: i.Parent.GetSharedCounterSetName(),
		Counters:   addCountersForMemSlices(capacitiesToCounters(i.PartCapacities()), int(i.MemorySlices.Start), int(i.MemorySlices.Size)),
	}}
}

// A variant of the legacy `GetDevice()`, for the Partitionable Devices paradigm.
func (d *AllocatableDevice) PartGetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.PartGetDevice()
	case MigStaticDeviceType:
		panic("PartGetDevice() called for MigStaticDeviceType")
	case MigDynamicDeviceType:
		return d.MigDynamic.PartGetDevice()
	case VfioDeviceType:
		panic("not yet implemented")
	}
	panic("unexpected type for AllocatableDevice")
}

// Insert one counter for each memory slice consumed, as given by the `start`
// and `size` parameters (from a nvml.GpuInstancePlacement). Mutate the input
// map in place, and (also) return it.
func addCountersForMemSlices(counters map[string]resourceapi.Counter, start int, size int) map[string]resourceapi.Counter {
	for i := start; i < start+size; i++ {
		counters[memsliceCounterName(i)] = resourceapi.Counter{Value: *resource.NewQuantity(1, resource.BinarySI)}
	}
	return counters
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

func placementString(p *nvml.GpuInstancePlacement) string {
	sfx := fmt.Sprintf("%d", p.Start)
	if p.Size > 1 {
		sfx = fmt.Sprintf("%s-%d", sfx, p.Start+p.Size-1)
	}
	// first, I had `placement-` in there -- but that may be too bulky
	//return toRFC1123Compliant(fmt.Sprintf("%s", sfx))
	return toRFC1123Compliant(sfx)
}

// `profile string` must be what's returned by profile.String(), the
// classical/canonical profile string notation, with a dot. This must not crash
// when fed with data from a MigDeviceInfo object deserialized from JSON.
func migppCanonicalName(parentMinor int, profile string, p *nvml.GpuInstancePlacement) string {
	placementSuffix := placementString(p)
	// Remove the dot in e.g. `4g.95gb` -- device names must not contain dots,
	// and this is used in a device name.
	profname := strings.ReplaceAll(profile, ".", "")
	return toRFC1123Compliant(fmt.Sprintf("gpu-%d-mig-%s-%s", parentMinor, profname, placementSuffix))
}
