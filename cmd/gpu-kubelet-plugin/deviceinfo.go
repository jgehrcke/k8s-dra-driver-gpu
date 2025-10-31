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

	"github.com/Masterminds/semver"
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

type GpuInfo struct {
	UUID                  string `json:"uuid"`
	index                 int
	minor                 int
	migEnabled            bool
	vfioEnabled           bool
	memoryBytes           uint64
	productName           string
	brand                 string
	architecture          string
	cudaComputeCapability string
	driverVersion         string
	cudaDriverVersion     string
	pcieBusID             string
	pcieRootAttr          *deviceattribute.DeviceAttribute
	migProfiles           []*MigProfileInfo
	MigCapable            bool

	// Properties that can only be known after inspecting MIG profiles
	maxCapacities PartCapacityMap
	memSliceCount int
}

// GpuInfo holds all of the relevant information about a GPU.
// type GpuInfo struct {
// 	Minor                 int
// 	UUID                  string
// 	MemoryBytes           uint64
// 	ProductName           string
// 	Brand                 string
// 	Architecture          string
// 	CudaComputeCapability string
// 	MigCapable            bool
// }

type PartCapacityMap map[resourceapi.QualifiedName]resourceapi.DeviceCapacity

type MigDeviceInfo struct {
	UUID    string `json:"uuid"`
	profile string

	parent        *GpuInfo
	placement     *MigDevicePlacement
	giProfileInfo *nvml.GpuInstanceProfileInfo
	giInfo        *nvml.GpuInstanceInfo
	ciProfileInfo *nvml.ComputeInstanceProfileInfo
	ciInfo        *nvml.ComputeInstanceInfo
	pcieBusID     string
	pcieRootAttr  *deviceattribute.DeviceAttribute
}

type VfioDeviceInfo struct {
	UUID                   string `json:"uuid"`
	deviceID               string
	vendorID               string
	index                  int
	parent                 *GpuInfo
	productName            string
	pcieBusID              string
	pcieRootAttr           *deviceattribute.DeviceAttribute
	numaNode               int
	iommuGroup             int
	addressableMemoryBytes uint64
}

type MigProfileInfo struct {
	profile    nvdev.MigProfile
	placements []*MigDevicePlacement
}

type MigDevicePlacement struct {
	nvml.GpuInstancePlacement
}

func (p MigProfileInfo) String() string {
	return p.profile.String()
}

func (d *GpuInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d", d.minor)
}

func (d *GpuInfo) String() string {
	return fmt.Sprintf("%s-%s", d.CanonicalName(), d.UUID)
}

func (d *MigDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-mig-%d-%d-%d", d.parent.minor, d.giInfo.ProfileId, d.placement.Start, d.placement.Size)
}

func (d *VfioDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-vfio-%d", d.index)
}

func (d *GpuInfo) PartDevAttributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
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
	}
}

// Full device capacity, based on looking at all MIG profiles.
func (d *GpuInfo) PartCapacities() PartCapacityMap {
	return d.maxCapacities
}

func (d *GpuInfo) GetSharedCounterSetName() string {
	return toRFC1123Compliant(fmt.Sprintf("gpu-%d-counter-set", d.index))
	//return toRFC1123Compliant(fmt.Sprintf("gpu-%s-counter-set", d.UUID))
}

// For now, define exactly one counter set per full GPU device. Individual
// partitions consume from that.
//
// In that counter set, define one counter per device capacity dimension, and
// add one counter (capacity 1) per memory slice.
func (d *GpuInfo) PartSharedCounterSets() []resourceapi.CounterSet {
	return []resourceapi.CounterSet{{
		Name:     d.GetSharedCounterSetName(),
		Counters: addCountersForMemSlices(capacitiesToCounters(d.maxCapacities), 0, d.memSliceCount),
	}}
}

func (d *GpuInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	// Let the full device consume everything. Goals: 1) when the full device is
	// allocated, all available counters drop to zero. 2) when the smallest
	// partition gets allocated, the full device cannot be allocated anymore.
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: d.GetSharedCounterSetName(),
		Counters:   addCountersForMemSlices(capacitiesToCounters(d.maxCapacities), 0, d.memSliceCount),
	}}
}

// For the new partitionable devices API, return the 'full' device announcement.
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

// Add detail that is only known after inspecting the individual MIG profiles.
func (d *GpuInfo) AddDetailAfterWalkingMigProfiles(maxcap PartCapacityMap, memSliceCount int) {
	d.maxCapacities = maxcap
	d.memSliceCount = memSliceCount
}

func (d *GpuInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: d.PartDevAttributes(),
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
			},
		},
	}
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}
	return device
}

func (d *MigDeviceInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(MigDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"parentUUID": {
				StringValue: &d.parent.UUID,
			},
			"profile": {
				StringValue: &d.profile,
			},
			"productName": {
				StringValue: &d.parent.productName,
			},
			"brand": {
				StringValue: &d.parent.brand,
			},
			"architecture": {
				StringValue: &d.parent.architecture,
			},
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.parent.cudaComputeCapability).String()),
			},
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.parent.driverVersion).String()),
			},
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.parent.cudaDriverVersion).String()),
			},
			"pcieBusID": {
				StringValue: &d.pcieBusID,
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"multiprocessors": {
				Value: *resource.NewQuantity(int64(d.giProfileInfo.MultiprocessorCount), resource.BinarySI),
			},
			"copyEngines": {Value: *resource.NewQuantity(int64(d.giProfileInfo.CopyEngineCount), resource.BinarySI)},
			"decoders":    {Value: *resource.NewQuantity(int64(d.giProfileInfo.DecoderCount), resource.BinarySI)},
			"encoders":    {Value: *resource.NewQuantity(int64(d.giProfileInfo.EncoderCount), resource.BinarySI)},
			"jpegEngines": {Value: *resource.NewQuantity(int64(d.giProfileInfo.JpegCount), resource.BinarySI)},
			"ofaEngines":  {Value: *resource.NewQuantity(int64(d.giProfileInfo.OfaCount), resource.BinarySI)},
			// Should we expose this as memoryBytes instead? Seemingly, in the
			// k8s landscape, that ship has long saileed: container limits for
			// example also use `memory`.
			"memory": {Value: *resource.NewQuantity(int64(d.giProfileInfo.MemorySizeMB*1024*1024), resource.BinarySI)},
		},
	}
	for i := d.placement.Start; i < d.placement.Start+d.placement.Size; i++ {
		capacity := resourceapi.QualifiedName(fmt.Sprintf("memorySlice%d", i))
		device.Capacity[capacity] = resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(1, resource.BinarySI),
		}
	}
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}
	return device
}

func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(VfioDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"deviceID": {
				StringValue: &d.deviceID,
			},
			"vendorID": {
				StringValue: &d.vendorID,
			},
			"numa": {
				IntValue: ptr.To(int64(d.numaNode)),
			},
			"pcieBusID": {
				StringValue: &d.pcieBusID,
			},
			"productName": {
				StringValue: &d.productName,
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"addressableMemory": {
				Value: *resource.NewQuantity(int64(d.addressableMemoryBytes), resource.BinarySI),
			},
		},
	}
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}
	return device
}
