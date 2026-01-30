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
	"slices"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/constraints"
)

// Naming convention: this name is used for announcement (device name announced
// in a DRA ResourceSlice), and it's also played back to us upon a request,
// which is when we look it up in the AllocatableDevices map. Conceptually, this
// is hence the same as kubeletplugin.Device.DeviceName (documented with
// 'DeviceName identifies the device inside that pool')
type DeviceName = string

type AllocatableDevices map[DeviceName]*AllocatableDevice

// AllocatableDevice represents an individual device that can be allocated.
// This can either be a full GPU or MIG device, but not both.
type AllocatableDevice struct {
	Gpu        *GpuInfo
	MigDynamic *MigSpec
	MigStatic  *MigDeviceInfo
	Vfio       *VfioDeviceInfo
}

type AllocatableDeviceList []*AllocatableDevice

func (d AllocatableDevice) Type() string {
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.MigDynamic != nil {
		return MigDynamicDeviceType
	}
	if d.MigStatic != nil {
		return MigStaticDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d AllocatableDevice) IsStaticOrDynMigDevice() bool {
	switch d.Type() {
	case MigStaticDeviceType, MigDynamicDeviceType:
		return true
	default:
		return false
	}
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigStaticDeviceType:
		return d.MigStatic.CanonicalName()
	case MigDynamicDeviceType:
		return d.MigDynamic.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.GetDevice()

	case MigStaticDeviceType:
		return d.MigStatic.GetDevice()
	case VfioDeviceType:
		return d.Vfio.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

// Note: the UUID is not used for announcing a device. Note that since
// introduction of DynamicMIG, some allocatable devices are abstract devices
// that do not have a UUID before actualization anyway.
func (d AllocatableDevice) UUID() string {
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.MigStatic != nil {
		return d.MigStatic.UUID
	}
	if d.MigDynamic != nil {
		// The caller must sure to never call UUID() hat we never get here. An
		// abstract MIG device must be prepared (created) -- only then, its UUID
		// can be determined. This method still exists because when the
		// DynamicMIG feature gate is disabled, `AllocatableDevices` _can_
		// implement UUIDProvider. This needs restructuring, to be safer.
		panic("unexpected call to UUID for AllocatableDevice type MigDynamic")
		//return ""
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	panic("unexpected type for AllocatableDevice")
}

// Required for implementing UUIDProvider. Meant to return full GPU UUIDs and
// MIG device UUIDs. Must not be used when the DynamicMIG featuregate is
// enabled. Unsure what it's supposed to return for VFIO devices.
func (d AllocatableDevices) UUIDs() []string {
	var uuids []string
	for _, dev := range d {
		uuids = append(uuids, dev.UUID())
	}
	return uuids
}

// Required for implementing UUIDProvider. Meant to return (only) full GPU UUIDs.
func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, dev := range d {
		if dev.Type() == GpuDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	return uuids
}

// Required for implementing UUIDProvider. Meant to return MIG device GPU UUIDs.
// Must not be called when the DynamicMIG featuregate is enabled.
func (d AllocatableDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, dev := range d {
		if dev.Type() == MigStaticDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	return uuids
}

func (d AllocatableDevices) getDevicesByGPUPCIBusID(pcieBusID string) AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		switch device.Type() {
		case GpuDeviceType:
			if device.Gpu.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigStaticDeviceType:
			if device.MigStatic.parent.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigDynamicDeviceType:
			if device.MigDynamic.Parent.pcieBusID == pcieBusID {
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

func (d AllocatableDevices) GetVfioDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// TODO: document the relevance of this method -- where are these names used,
// what are guarantees about them? How does this relate to the UUID concept?
func (d AllocatableDevices) PossibleMigDeviceNames() []string {
	var names []string
	for _, device := range d {
		if device.Type() == MigStaticDeviceType {
			names = append(names, device.MigStatic.CanonicalName())
		}
		if device.Type() == MigDynamicDeviceType {
			names = append(names, device.MigDynamic.CanonicalName())
		}
	}
	slices.Sort(names)
	return names
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

// Returns the names of all allocatable devices, by calling into CanonicalName()
// for each device. Before the introduction of dynamic MIG this returned the
// UUID, but we made the transition to first-classing minor numbers a while ago.
// Also see https://github.com/NVIDIA/k8s-dra-driver-gpu/pull/428 and
// https://github.com/NVIDIA/k8s-dra-driver-gpu/issues/427#issuecomment-3069573022
func (d AllocatableDevices) Names() []string {
	var names []string
	for _, device := range d {
		names = append(names, device.CanonicalName())
	}
	slices.Sort(names)
	return names
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

func (d AllocatableDevices) RemoveSiblingDevices(device *AllocatableDevice) {
	var pciBusID string
	switch device.Type() {
	case GpuDeviceType:
		pciBusID = device.Gpu.pcieBusID
	case VfioDeviceType:
		pciBusID = device.Vfio.pcieBusID
	case MigStaticDeviceType:
		// TODO: Implement once dynamic MIG is supported.
		return
	case MigDynamicDeviceType:
		// TODO
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
		case MigStaticDeviceType:
			// TODO
			continue
		case MigDynamicDeviceType:
			// TODO
			continue
		}
	}
}

func (d *AllocatableDevice) IsHealthy() bool {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.Health == Healthy
	case MigStaticDeviceType:
		// TODO: review -- what about the parent?
		return d.MigStatic.Health == Healthy
	case MigDynamicDeviceType:
		// TODO. For now, pretend health -- this device maybe hasn't manifested
		// yet. Or has it? We could adopt the health status of the parent, but
		// that's also not meaningful I think.
		return true
	}
	panic("unexpected type for AllocatableDevice")
}
