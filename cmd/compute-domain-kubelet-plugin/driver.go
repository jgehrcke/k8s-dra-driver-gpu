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
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	resourceapi "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/workqueue"
)

// ErrorRetryMaxTimeout is the max amount of time we retry when errors are
// returned before giving up.
const ErrorRetryMaxTimeout = 45 * time.Second

// permanentError defines an error indicating that it is permanent.
// By default, every error will be retried up to ErrorRetryMaxTimeout.
// Errors marked as permanent will not be retried.
type permanentError struct{ error }

func isPermanentError(err error) bool {
	return errors.As(err, &permanentError{})
}

type driver struct {
	client       coreclientset.Interface
	pluginhelper *kubeletplugin.Helper
	state        *DeviceState
	pulock       *Flock
}

type Flock struct {
	path       string
	PollPeriod time.Duration
}

func NewFlock(path string) *Flock {
	return &Flock{
		path: path,
		// Short default period to keep lock acquisition rather responsive;
		// adjust on a per-use case basis.
		PollPeriod: 200 * time.Millisecond,
	}
}

// Acquire attempts to acquire an exclusive file lock, polling with the configured
// PollPeriod until whichever comes first:
//
// - lock successfully acquired
// - timeout (if provided)
// - external cancellation of context
//
// Returns a release function that must be called to unlock the file, typically
// with defer().
//
// Introduced to protect the work in nodePrepareResource() and
// nodeUnprepareResource() under a file-based lock because more than one driver
// pod may be running on a node, but at most one such function must execute at
// any given time.
func (l *Flock) Acquire(ctx context.Context, timeout ...time.Duration) (func(), error) {
	f, oerr := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0644)
	if oerr != nil {
		return nil, fmt.Errorf("error opening lock file (%s): %w", l.path, oerr)
	}

	t0 := time.Now()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	// Use non-blocking peek with LOCK_NB flag and polling; trade-off:
	//
	// - pro: no need for having to reliably cancel a potentially long-blocking
	//   flock() system call (can only be done with signals).
	// - con: lock acquisition time after a release is not immediate, but may
	//   take up to PollPeriod amount of time. Not an issue in this context.
	for {
		flerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flerr == nil {
			// Lock acquired. Return release function. An exclusive flock() lock
			// gets released when its file descriptor gets closed (also true
			// when the lock-holding process crashes).
			return func() {
				f.Close()
			}, nil
		}

		if flerr != syscall.EWOULDBLOCK {
			// May be EBADF, EINTR, EINVAl, ENOLCK, and
			// in general we want an outer retry mechanism
			// to retry in view of any of those.
			f.Close()
			return nil, fmt.Errorf("error acquiring lock (%s): %w", l.path, flerr)
		}

		// Lock is currently held by other entity. Check for exit criteria;
		// otherwise retry lock acquisition upon next tick.

		if len(timeout) > 0 {
			if time.Since(t0) >= timeout[0] {
				f.Close()
				return nil, fmt.Errorf("timeout acquiring lock (%s)", l.path)
			}
		}

		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
			// Retry flock().
		}
	}
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}

	driver := &driver{
		client: config.clientsets.Core,
		state:  state,
		pulock: NewFlock(DriverPluginPath + "/pu.lock"),
	}

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		// By default, the DRA library serializes (un)prepare calls. That is, at
		// most one such call is exposed to the driver at any given time.
		// Disable that behavior: this driver has codependent prepare() actions
		// (where for the first prepare() to eventually complete, a second
		// prepare() must be incoming). Concurrency management for incoming
		// requests is done with this driver's work queue abstraction.
		kubeletplugin.Serialize(false),
	)
	if err != nil {
		return nil, err
	}
	driver.pluginhelper = helper

	// Enumerate the set of ComputeDomain daemon devices and publish them
	var resourceSlice resourceslice.Slice
	for _, device := range state.allocatable {
		// Explicitly exclude ComputeDomain channels from being advertised here. They
		// are instead advertised in as a network resource from the control plane.
		if device.Type() == ComputeDomainChannelType && device.Channel.ID != 0 {
			continue
		}
		resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice())
	}

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			config.flags.nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}

	if err := state.computeDomainManager.Start(ctx); err != nil {
		return nil, err
	}

	if err := driver.pluginhelper.PublishResources(ctx, resources); err != nil {
		return nil, err
	}

	return driver, nil
}

func (d *driver) Shutdown() error {
	if d == nil {
		return nil
	}
	if err := d.state.computeDomainManager.Stop(); err != nil {
		return fmt.Errorf("error stopping ComputeDomainManager: %w", err)
	}
	d.pluginhelper.Stop()
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(6).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)
	workQueue := workqueue.New(workqueue.DefaultControllerRateLimiter())
	results := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		wg.Add(1)
		workQueue.EnqueueRaw(claim, func(ctx context.Context, obj any) error {
			done, res := d.nodePrepareResource(ctx, claim)
			if done {
				results[claim.UID] = res
				wg.Done()
				return nil
			}
			return fmt.Errorf("%w", res.Err)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)
	return results, nil
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(6).Infof("UnprepareResourceClaims called with %d claim(s)", len(claimRefs))

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)
	workQueue := workqueue.New(workqueue.DefaultControllerRateLimiter())
	results := make(map[types.UID]error)

	for _, claim := range claimRefs {
		wg.Add(1)
		workQueue.EnqueueRaw(claim, func(ctx context.Context, obj any) error {
			done, err := d.nodeUnprepareResource(ctx, claim)
			if done {
				results[claim.UID] = err
				wg.Done()
				return nil
			}
			return fmt.Errorf("%w", err)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)

	return results, nil
}

func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) (bool, kubeletplugin.PrepareResult) {
	release, err := d.pulock.Acquire(ctx, 10*time.Second)
	if err != nil {
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error acquiring prep/unprep lock: %w", err),
		}
		return false, res
	}
	defer release()

	if claim.Status.Allocation == nil {
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("no allocation set in ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return true, res
	}

	devs, err := d.state.Prepare(ctx, claim)
	if err != nil {
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
		return isPermanentError(err), res
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, devs)
	return true, kubeletplugin.PrepareResult{Devices: devs}
}

func (d *driver) nodeUnprepareResource(ctx context.Context, claimRef kubeletplugin.NamespacedObject) (bool, error) {
	release, err := d.pulock.Acquire(ctx, 10*time.Second)
	if err != nil {
		return false, fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()

	if err := d.state.Unprepare(ctx, claimRef); err != nil {
		return isPermanentError(err), fmt.Errorf("error unpreparing devices for claim '%v': %w", claimRef.String(), err)
	}

	klog.Infof("unprepared devices for claim '%v'", claimRef.String())
	return true, nil
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
