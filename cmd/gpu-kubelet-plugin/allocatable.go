/*
 * Copyright (c) 2022-2025, NVIDIA CORPORATION.  All rights reserved.
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
	"slices"
	"strings"

	"github.com/Masterminds/semver"
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/constraints"
	"k8s.io/utils/ptr"
)

// Naming convention: this name is used for announcement (device name announced
// in a DRA ResourceSlice), and it's also played back to us upon a request,
// which is when we look it up in the AllocatableDevices map. Conceptually, this
// is hence the same as kubeletplugin.Device.DeviceName (documented with
// 'DeviceName identifies the device inside that pool')
type DeviceName = string

type AllocatableDevices map[DeviceName]*AllocatableDevice

//type AllocatableDevices []AllocatableDevice

// AllocatableDevice represents an individual device that can be allocated.
// This can either be a full GPU or MIG device, but not both.
type AllocatableDevice struct {
	Gpu  *GpuInfo
	Mig  *MigInfo
	Vfio *VfioDeviceInfo
}

// MigInfo encodes profile and placement. Maybe rename to AbstractMigInfo or
// AbstractMigDevice because this does not encode a materialized device.
type MigInfo struct {
	Parent        *GpuInfo
	Profile       nvdev.MigProfile
	GIProfileInfo nvml.GpuInstanceProfileInfo
	// JP: rename to Placement?
	MemorySlices nvml.GpuInstancePlacement
}

func (i *MigInfo) CanonicalName() DeviceName {
	return migppCanonicalName(i.Parent, i.Profile.String(), &i.MemorySlices)
}

// Note: this should be the same regardless of the placement.
func (i MigInfo) PartCapacities() PartCapacityMap {
	p := i.GIProfileInfo
	return PartCapacityMap{
		"multiprocessors": intcap(p.MultiprocessorCount),
		"copyEngines":     intcap(p.CopyEngineCount),
		"decoders":        intcap(p.DecoderCount),
		"encoders":        intcap(p.EncoderCount),
		"jpegEngines":     intcap(p.JpegCount),
		"ofaEngines":      intcap(p.OfaCount),
		"memory":          intcap(int64(p.MemorySizeMB * 1024 * 1024)), // Encode unit in name? 'bytes'
	}
}

func (i MigInfo) PartAttributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(MigDeviceType),
		},
		"parentUUID": {
			StringValue: &i.Parent.UUID,
		},
		"parentIndex": {
			IntValue: ptr.To(int64(i.Parent.index)), // TODO: expose?
		},
		"parentMinor": {
			IntValue: ptr.To(int64(i.Parent.minor)), // TODO: expose?
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
		counters[toRFC1123Compliant(string(name))] = resourceapi.Counter{Value: cap.Value}
	}
	return counters
}

func (i MigInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	// Each entry in capacity is also modeled as consumable counter (consuming
	// from the parent device). In addition, this MIG device, if allocated,
	// consumes at least one specific memory slice. Each memory slice is modeled
	// with its own counter (capacity: 1). Note that for example on a B200 GPU,
	// the `3g.90gb` device consumes 4 out of 8 memory slices in total, but only
	// 3 out of seven SMs. That is, with two `3g.90gb` devices allocated all
	// memory slices are consumed, and one SM -- while unallocated -- cannot be
	// used anymore.
	return []resourceapi.DeviceCounterConsumption{{
		// The parent is a full GPU device. When this device is allocated, it
		// consumes from the parent's CounterSet. The parent's counter set is
		// referred to by name. Use a naming convention: currently, a full GPU
		// has precisely one counter set associated with it, and its name has
		// the form 'gpu-%d-counter-set' where the placeholder is the GPU index
		// (change to UUID)?
		CounterSet: i.Parent.GetSharedCounterSetName(),

		Counters: addCountersForMemSlices(capacitiesToCounters(i.PartCapacities()), int(i.MemorySlices.Start), int(i.MemorySlices.Size)),
	}}
}

// Insert one counter for each memory slice consumed, as given by the `start`
// and `size` parameters (nvml.GpuInstancePlacement). Mutate the input map in
// place, and (also) return it.
func addCountersForMemSlices(counters map[string]resourceapi.Counter, start int, size int) map[string]resourceapi.Counter {
	for i := start; i < start+size; i++ {
		counters[memsliceCounterName(i)] = resourceapi.Counter{Value: *resource.NewQuantity(1, resource.BinarySI)}
	}
	return counters
}

func (i *MigInfo) PartGetDevice() resourceapi.Device {
	d := resourceapi.Device{
		Name:             i.CanonicalName(),
		Attributes:       i.PartAttributes(),
		Capacity:         i.PartCapacities(),
		ConsumesCounters: i.PartConsumesCounters(),
	}
	return d
}

func (d AllocatableDevice) Type() string {
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.Mig != nil {
		return MigDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigDeviceType:
		return d.Mig.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) PartGetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.PartGetDevice()
	case MigDeviceType:
		return d.Mig.PartGetDevice()
	case VfioDeviceType:
		return d.Vfio.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

// Used for announcing / describing a device, possibly pre-allocation. That is,
// for dynamic MIG devices, we may not want to describe such devices with a
// UUID.
func (d AllocatableDevice) UUID() string {
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.Mig != nil {
		// Review: this can only be done after dynamic MIG device creation, once
		// this ID exists.
		return "foo"
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	panic("unexpected type for AllocatableDevice")
}

type AllocatableDeviceList []*AllocatableDevice

func (d AllocatableDevices) getDevicesByGPUPCIBusID(pcieBusID string) AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		switch device.Type() {
		case GpuDeviceType:
			if device.Gpu.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigDeviceType:
			if device.Mig.Parent.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case VfioDeviceType:
			if device.Vfio.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		}
	}
	return devices
}

func (d AllocatableDevices) GetGPUByPCIeBusID(pcieBusID string) *AllocatableDevice {
	for _, device := range d {
		if device.Type() != GpuDeviceType {
			continue
		}
		if device.Gpu.pcieBusID == pcieBusID {
			return device
		}
	}
	return nil
}

func (d AllocatableDevices) GetGPUs() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetMigDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == MigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetVfioDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			uuids = append(uuids, device.Gpu.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) PossibleMigDeviceNames() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == MigDeviceType {
			uuids = append(uuids, device.Mig.CanonicalName())
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			uuids = append(uuids, device.Vfio.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) Names() []string {
	names := append(d.GpuUUIDs(), d.PossibleMigDeviceNames()...)
	slices.Sort(names)
	return names
}

// toRFC1123Compliant converts the input to a DNS name compliant with RFC 1123.
// Note that a device name in DRA must not contain dots either, so let's just
// remove them?
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
	return toRFC1123Compliant(fmt.Sprintf("%s", sfx))
}

// `profile string` must be what's returned by profile.String(), the
// classical/canonical profile string notation, with a dot.
func migppCanonicalName(parent *GpuInfo, profile string, p *nvml.GpuInstancePlacement) string {
	placementSuffix := placementString(p)
	// Remove the dot in e.g. `4g.95gb` -- device names must not contain dots,
	// and this is used in a device name.
	profname := strings.ReplaceAll(profile, ".", "")
	return toRFC1123Compliant(fmt.Sprintf("gpu-%d-mig-%s-%s", parent.minor, profname, placementSuffix))
}

// Return canonical name for memory slice (placement) `i` (a zero-based index).
// Note that this name must be used for memslice-N counters in a SharedCounters
// counter set, and for corresponding counters in a ConsumesCounters counter
// set. Counters (as opposed to capacities) are allowed to have hyphens in their
// name.
func memsliceCounterName(i int) string {
	return fmt.Sprintf("memory-slice-%d", i)
}

// Helper for creating an integer-based DeviceCapacity. Accept any integer type.
func intcap[T constraints.Integer](i T) resourceapi.DeviceCapacity {
	return resourceapi.DeviceCapacity{Value: *resource.NewQuantity(int64(i), resource.BinarySI)}
}

func (d AllocatableDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	uuids = append(uuids, d.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) RemoveSiblingDevices(device *AllocatableDevice) {
	var pciBusID string
	switch device.Type() {
	case GpuDeviceType:
		pciBusID = device.Gpu.pcieBusID
	case VfioDeviceType:
		pciBusID = device.Vfio.pcieBusID
	case MigDeviceType:
		// TODO: Implement once dynamic MIG is supported.
		return
	}

	siblings := d.getDevicesByGPUPCIBusID(pciBusID)
	for _, sibling := range siblings {
		if sibling.Type() == device.Type() {
			continue
		}
		switch sibling.Type() {
		case GpuDeviceType:
			delete(d, sibling.Gpu.CanonicalName())
		case VfioDeviceType:
			delete(d, sibling.Vfio.CanonicalName())
		case MigDeviceType:
			// TODO: Implement once dynamic MIG is supported.
			continue
		}
	}
}
