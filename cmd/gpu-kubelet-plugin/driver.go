/*
 * Copyright (c) 2022-2024, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/flock"
)

// DriverPrepUprepFlockPath is the path to a lock file used to make sure
// that calls to nodePrepareResource() / nodeUnprepareResource() never
// interleave, node-globally.
const DriverPrepUprepFlockFileName = "pu.lock"

type driver struct {
	client       coreclientset.Interface
	pluginhelper *kubeletplugin.Helper
	state        *DeviceState
	pulock       *flock.Flock
	healthcheck  *healthcheck
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}

	puLockPath := filepath.Join(config.DriverPluginPath(), DriverPrepUprepFlockFileName)

	driver := &driver{
		client: config.clientsets.Core,
		state:  state,
		pulock: flock.NewFlock(puLockPath),
	}

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		kubeletplugin.Serialize(false),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	)
	if err != nil {
		return nil, err
	}
	driver.pluginhelper = helper

	// Note(JP): I first considered model 1, and then implemented model model 2.
	//
	// Model 1) Create G+1 resource slices, given G physical GPUs. 1) one slice
	// with shared counters, for each full GPU 2) for each full GPU, a slice
	// with the full-GPU-device and all potential MIG profile/placement devices.
	// That model is inspired by KEP 4815 which talks about "require that
	// ResourceSlice objects can only contain either SharedCounters or Devices".
	// In practice, that does not seem to be a requirement (yet?); in fact, it
	// doesn't seem to work to consume from a shared counter _not_ specified in
	// the same ResourceSlice. Which brings me to model 2.
	//
	// Model 2) Create G resource slices, given G physical GPUs. Each slice
	// describes the abstract full device capacity by definining _one_ counter
	// set as part of `SharedCounters` (there could be 8 counter sets in total,
	// but try to get away using one). Then, in the same resource slice define
	// all devices allocatable for that physical GPU. That is, M possible MIG
	// devices, and 1 device representing the full GPU. Hence:
	//
	// - G resource slices
	// - Each resource slice:
	//       - Defines `SharedCounters` with 1 counter set
	//       - Defines M+1 devices
	//
	//
	//
	// Specific quote from KEP 4815 does does not make sense to be right now:
	// "The decision to require that ResourceSlice objects can only contain
	// either SharedCounters or Devices was made to prevent having to enforce
	// overly strict validation to make sure that ResourceSlice objects can't
	// exceed the etcd limit."

	var slice resourceslice.Slice
	countersets := []resourceapi.CounterSet{}

	// Iterate through allocatable devices, in stable sort order (by device
	// name) This makes the order of devices presented in a resource slice
	// predictable (I hope).
	for _, devname := range slices.Sorted(maps.Keys(state.allocatable)) {
		device := state.allocatable[devname]
		klog.V(4).Infof("About to announce device %s", devname)

		//for _, device := range state.allocatable.SortedPairs() {
		// Full GPU: take note of countersets, indicating absolute capacity.

		if device.Gpu != nil {
			countersets = append(countersets, device.Gpu.PartSharedCounterSets()...)
		}

		// Add all allocatable devices; this includes not-yet-manifested MIG
		// devices
		slice.Devices = append(slice.Devices, device.PartGetDevice())
	}

	// I tried having the sharedCounters in their own slice, and that doesn't seem to work.
	slice.SharedCounters = countersets

	// var resourceSlice resourceslice.Slice
	// for _, device := range state.allocatable {
	// 	resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice())
	// }

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			config.flags.nodeName: {Slices: []resourceslice.Slice{slice}},
		},
	}

	healthcheck, err := startHealthcheck(ctx, config, helper)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}
	driver.healthcheck = healthcheck

	// Note(JP): from KEP 4815: "we will add client-side validation in the
	// ResourceSlice controller helper, so that any errors in the ResourceSlices
	// will be caught before they even are applied to the APIServer" -- the
	// helper below is being referred to.
	//
	// This will be a TODO soon:
	// https://github.com/kubernetes/kubernetes/commit/a171795e313ee9f407fef4897c1a1e2052120991

	if err := driver.pluginhelper.PublishResources(ctx, resources); err != nil {
		return nil, err
	}

	klog.V(4).Infof("Current kubelet plugin registration status: %s", helper.RegistrationStatus())

	return driver, nil
}

func (d *driver) Shutdown() error {
	if d == nil {
		return nil
	}

	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}

	d.pluginhelper.Stop()
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {

	if len(claims) == 0 {
		// That's probably the health check, log that on higher verbosity level
		klog.V(7).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))
	} else {
		// Log canonical string representation for each claim injected here --
		// we've noticed that this can greatly facilitate debugging.
		var rcs []string
		for _, c := range claims {
			rcs = append(rcs, ResourceClaimToString(c))
		}
		klog.V(6).Infof("PrepareResourceClaims called with %d claim(s): %v", len(claims), rcs)
	}

	results := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		results[claim.UID] = d.nodePrepareResource(ctx, claim)
	}

	return results, nil
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(6).Infof("UnprepareResourceClaims called with %d claim(s)", len(claimRefs))

	results := make(map[types.UID]error)

	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.nodeUnprepareResource(ctx, claimRef)
	}

	return results, nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	// For now we just follow the advice documented in the DRAPlugin API docs.
	// See: https://pkg.go.dev/k8s.io/apimachinery/pkg/util/runtime#HandleErrorWithContext
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error acquiring prep/unprep lock: %w", err),
		}
	}
	defer release()

	devs, err := d.state.Prepare(ctx, claim)

	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, devs)
	return kubeletplugin.PrepareResult{Devices: devs}
}

func (d *driver) nodeUnprepareResource(ctx context.Context, claimNs kubeletplugin.NamespacedObject) error {
	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()

	if err := d.state.Unprepare(ctx, string(claimNs.UID)); err != nil {
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claimNs.UID, err)
	}

	return nil
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
//
// TODO: implement loop to remove mpsControlDaemon folders from the mps
//       path for claimUIDs that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupMpsControlDaemonArtifacts(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
