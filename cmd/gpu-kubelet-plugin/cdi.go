/*
 * Copyright (c) 2022-2023, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/sirupsen/logrus"

	nvdevice "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	transformroot "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/transform/root"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	cdiVendor = "k8s." + DriverName

	//cdiDeviceClass = "device"
	//cdiDeviceKind  = cdiVendor + "/" + cdiDeviceClass
	cdiClaimClass = "claim"
	//cdiClaimKind  = cdiVendor + "/" + cdiClaimClass

	cdiBaseSpecIdentifier = "base"
	cdiVfioSpecIdentifier = "vfio"
	//cdiBaseSpecIdentifier = "base"

	defaultCDIRoot = "/var/run/cdi"
)

type CDIHandler struct {
	logger   *logrus.Logger
	nvml     nvml.Interface
	nvdevice nvdevice.Interface
	//nvcdiDevice       nvcdi.Interface
	nvcdiClaim        nvcdi.Interface
	cache             *cdiapi.Cache
	driverRoot        string
	devRoot           string
	targetDriverRoot  string
	nvidiaCDIHookPath string

	cdiRoot string
	vendor  string
	//deviceClass string
	claimClass string
}

func NewCDIHandler(opts ...cdiOption) (*CDIHandler, error) {
	h := &CDIHandler{}
	for _, opt := range opts {
		opt(h)
	}

	if h.logger == nil {
		h.logger = logrus.New()
		// h.logger.SetOutput(io.Discard)
	}
	if h.nvml == nil {
		h.nvml = nvml.New()
	}
	if h.cdiRoot == "" {
		h.cdiRoot = defaultCDIRoot
	}
	if h.nvdevice == nil {
		h.nvdevice = nvdevice.New(h.nvml)
	}
	if h.vendor == "" {
		h.vendor = cdiVendor
	}
	// if h.deviceClass == "" {
	// 	h.deviceClass = cdiDeviceClass
	// }
	if h.claimClass == "" {
		h.claimClass = cdiClaimClass
	}
	// if h.nvcdiDevice == nil {
	// 	nvcdilib, err := nvcdi.New(
	// 		nvcdi.WithDeviceLib(h.nvdevice),
	// 		nvcdi.WithDriverRoot(h.driverRoot),
	// 		nvcdi.WithDevRoot(h.devRoot),
	// 		nvcdi.WithLogger(h.logger),
	// 		nvcdi.WithNvmlLib(h.nvml),
	// 		nvcdi.WithMode("nvml"),
	// 		nvcdi.WithVendor(h.vendor),
	// 		nvcdi.WithClass(h.deviceClass),
	// 		nvcdi.WithNVIDIACDIHookPath(h.nvidiaCDIHookPath),
	// 	)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("unable to create CDI library for devices: %w", err)
	// 	}
	// 	h.nvcdiDevice = nvcdilib
	// }
	if h.nvcdiClaim == nil {
		nvcdilib, err := nvcdi.New(
			nvcdi.WithDeviceLib(h.nvdevice),
			nvcdi.WithDriverRoot(h.driverRoot),
			nvcdi.WithDevRoot(h.devRoot),
			nvcdi.WithLogger(h.logger),
			nvcdi.WithNvmlLib(h.nvml),
			nvcdi.WithMode("nvml"),
			nvcdi.WithVendor(h.vendor),
			nvcdi.WithClass(h.claimClass),
			nvcdi.WithNVIDIACDIHookPath(h.nvidiaCDIHookPath),
			nvcdi.WithFeatureFlags(nvcdi.FeatureDisableNvsandboxUtils),
			//vcdi.WithNvsandboxuitilsLib(nil),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create CDI library for claims: %w", err)
		}
		h.nvcdiClaim = nvcdilib
	}
	if h.cache == nil {
		cache, err := cdiapi.NewCache(
			cdiapi.WithSpecDirs(h.cdiRoot),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create a new CDI cache: %w", err)
		}
		h.cache = cache
	}

	return h, nil
}

func (cdi *CDIHandler) writeSpec(spec spec.Interface, specName string) error {
	// Transform the spec to make it aware that it is running inside a container.
	err := transformroot.New(
		transformroot.WithRoot(cdi.driverRoot),
		transformroot.WithTargetRoot(cdi.targetDriverRoot),
		transformroot.WithRelativeTo("host"),
	).Transform(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to transform driver root in CDI spec: %w", err)
	}

	// Update the spec to include only the minimum version necessary.
	minVersion, err := cdispec.MinimumRequiredVersion(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %w", err)
	}
	spec.Raw().Version = minVersion

	// Write the spec out to disk.
	return cdi.cache.WriteSpec(spec.Raw(), specName)
}

// func (cdi *CDIHandler) CreateStandardDeviceSpecFile(allocatable AllocatableDevices) error {
// 	if err := cdi.createStandardNvidiaDeviceSpecFile(allocatable); err != nil {
// 		klog.Errorf("failed to create standard nvidia device spec file: %v", err)
// 		return err
// 	}

// 	if featuregates.Enabled(featuregates.PassthroughSupport) {
// 		if err := cdi.createStandardVfioDeviceSpecFile(allocatable); err != nil {
// 			klog.Errorf("failed to create standard vfio device spec file: %v", err)
// 			return err
// 		}
// 	}
// 	return nil
// }

func (cdi *CDIHandler) CreateClaimSpecFile(claimUID string, preparedDevices PreparedDevices) error {
	// Initialize NVML in order to get the device edits.
	if r := cdi.nvml.Init(); r != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", r)
	}
	defer func() {
		if r := cdi.nvml.Shutdown(); r != nvml.SUCCESS {
			klog.Warningf("failed to shutdown NVML: %v", r)
		}
	}()

	// Generate those parts of the container spec that are note device-specific.
	// Inject things like driver library mounts and meta devices.

	// this may initialize nvsandboxutilslib under the hood
	klog.V(6).Infof("Call nvcdiDevice.GetCommonEdits()")
	commonEdits, err := cdi.nvcdiClaim.GetCommonEdits()

	if err != nil {
		return fmt.Errorf("failed to get common CDI spec edits: %w", err)
	}

	// Make sure that NVIDIA_VISIBLE_DEVICES is set to void to avoid the
	// nvidia-container-runtime honoring it in addition to the underlying
	// runtime honoring CDI.
	commonEdits.Env = append(
		commonEdits.Env,
		"NVIDIA_VISIBLE_DEVICES=void")

	var deviceSpecs []cdispec.Device

	for _, group := range preparedDevices {
		for _, dev := range group.Devices {
			duuid := ""

			// Construct claim-specific CDI device name in accordance with the
			// naming convention is encoded in GetClaimDeviceName() below.
			dname := fmt.Sprintf("%s-%s", claimUID, dev.CanonicalName())

			var dspec cdispec.Device
			if dev.Type() == VfioDeviceType {
				// Note(JP): overwrite commonEdits -- this wasn't used in the
				// original vfio dev PR, but shouldn't we also use
				// `cdi.nvcdiDevice.GetCommonEdits()` here? Also: assume that
				// all devices in `preparedDevices` are vfio devices.
				commonEdits = GetVfioCommonCDIContainerEdits()
				dspec = cdispec.Device{
					Name:           dname,
					ContainerEdits: *GetVfioCDIContainerEdits(dev.Vfio.Info).ContainerEdits,
				}

			} else if dev.Mig != nil {
				duuid = dev.Mig.Created.UUID
				dspecs, err := cdi.nvcdiClaim.GetDeviceSpecsByID(duuid)
				if err != nil {
					return fmt.Errorf("unable to get device spec for %s: %w", dname, err)
				}
				dspec = dspecs[0]
			} else {
				duuid = dev.Gpu.Info.UUID
				dspecs, err := cdi.nvcdiClaim.GetDeviceSpecsByID(duuid)
				if err != nil {
					return fmt.Errorf("unable to get device spec for %s: %w", dname, err)
				}
				dspec = dspecs[0]
			}

			// For a just-created MIG device I see this emit a msg on stderr:
			//
			// ERROR: migGetDevFileInfo 212 result=11ERROR: init 310
			// result=11ERROR: migGetDevFileInfo 212 result=11ERROR: init 310
			// result=11
			//
			// Evan said that "Seems like nvsandboxutils " "There is a feature
			// flag in the CDI API to disable it, but you may need a code change
			// in the driver ..." Probably triggered here:
			// https://github.com/NVIDIA/nvidia-container-toolkit/blob/e03ac3644d63ec30849dffebd0170811e4903e78/internal/platform-support/dgpu/nvsandboxutils.go#L67
			//

			// Note(JP): for a regular GPU, this canonical name is for example
			// `gpu-0`, with the numerical suffix as of the time of writing
			// reflecting the device minor. NVMLs' DeviceSetMigMode() is
			// documented with 'This API may unbind or reset the device to
			// activate the requested mode. Thus, the attributes associated with
			// the device, such as minor number, might change. The caller of
			// this API is expected to query such attributes again.' -- if the
			// minor is indeed not necessarily stable, there may be problems
			// associating this spec _long-term_ with that name. Maye always
			// dynamically generate spec during prepare().
			dspec.Name = dname
			deviceSpecs = append(deviceSpecs, dspec)
			klog.V(6).Infof("Device nodes about to inject for dev %s: %v", dname, len(dspec.ContainerEdits.DeviceNodes))

			// If there edits passed as part of the device config state (set on
			// the group), add them to the spec of each device in that group.
			if group.ConfigState.containerEdits != nil {
				deviceSpec := cdispec.Device{
					Name:           fmt.Sprintf("%s-%s", claimUID, dname),
					ContainerEdits: *group.ConfigState.containerEdits.ContainerEdits,
				}
				deviceSpecs = append(deviceSpecs, deviceSpec)
			}
		}
	}

	spec, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiClaimClass),
		spec.WithDeviceSpecs(deviceSpecs),
		spec.WithEdits(*commonEdits.ContainerEdits),
	)
	if err != nil {
		return fmt.Errorf("failed to create CDI spec: %w", err)
	}

	// Write the per-claim spec that was generated above to the filesystem. As
	// it is bound to a DRA ResourceClaim, it's transient (bound to the lifetime
	// of a container). Hence, Use the "transient spec" concept from CDI.
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	klog.V(6).Infof("Writing CDI spec '%s' for claim '%s'", specName, claimUID)
	return cdi.writeSpec(spec, specName)
}

func (cdi *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	klog.V(6).Infof("Delete CDI spec file: '%s', claim '%s'", specName, claimUID)
	return cdi.cache.RemoveSpec(specName)
}

// Philosophy: all devices to be injected into a container are defined in a
// single, transient CDI spec. This function returns the fully qualified
// identifier for a device defined in that spec. Example:
// k8s.gpu.nvidia.com/claim=dab5ab50-d59a-42a6-af16-cfd4628c0f7a-gpu-0
// That identifier can be used elsewhere, and _points to the spec_.
func (cdi *CDIHandler) GetClaimDeviceName(claimUID string, device *AllocatableDevice, containerEdits *cdiapi.ContainerEdits) string {
	return cdiparser.QualifiedName(cdiVendor, cdiClaimClass, fmt.Sprintf("%s-%s", claimUID, device.CanonicalName()))
}
