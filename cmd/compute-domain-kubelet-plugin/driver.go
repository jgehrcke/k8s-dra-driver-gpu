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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"

	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/workqueue"
)

// ErrorRetryMaxTimeout is the max amount of time we retry when errors are
// returned before giving up.
const ErrorRetryMaxTimeout = 45 * time.Second

// permanentError defines an error indicating that it is permanent.
// By default, every error will be retried up to ErrorRetryMaxTimeout.
// Errors marked as permament will not be retried.
type permanentError struct{ error }

func isPermanentError(err error) bool {
	return errors.As(err, &permanentError{})
}

var _ drapbv1.DRAPluginServer = &driver{}

type driver struct {
	sync.Mutex
	client coreclientset.Interface
	plugin kubeletplugin.DRAPlugin
	state  *DeviceState
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	driver := &driver{
		client: config.clientsets.Core,
	}

	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}
	driver.state = state

	plugin, err := kubeletplugin.Start(
		ctx,
		[]any{driver},
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		kubeletplugin.RegistrarSocketPath(PluginRegistrationPath),
		kubeletplugin.PluginSocketPath(DriverPluginSocketPath),
		kubeletplugin.KubeletPluginSocketPath(DriverPluginSocketPath))
	if err != nil {
		return nil, err
	}
	driver.plugin = plugin

	// Enumerate the set of ComputeDomain daemon devices and publish them
	var resources kubeletplugin.Resources
	for _, device := range state.allocatable {
		// Explicitly exclude ComputeDomain channels from being advertised here. They
		// are instead advertised in as a network resource from the control plane.
		if device.Type() == ComputeDomainChannelType && device.Channel.ID != 0 {
			continue
		}
		resources.Devices = append(resources.Devices, device.GetDevice())
	}

	if err := state.computeDomainManager.Start(ctx); err != nil {
		return nil, err
	}

	if err := plugin.PublishResources(ctx, resources); err != nil {
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
	d.plugin.Stop()
	return nil
}

func (d *driver) NodePrepareResources(ctx context.Context, req *drapbv1.NodePrepareResourcesRequest) (*drapbv1.NodePrepareResourcesResponse, error) {
	klog.Infof("NodePrepareResource is called: number of claims: %d", len(req.Claims))
	preparedResources := &drapbv1.NodePrepareResourcesResponse{Claims: map[string]*drapbv1.NodePrepareResourceResponse{}}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)
	workQueue := workqueue.New(workqueue.DefaultControllerRateLimiter())

	for _, claim := range req.Claims {
		wg.Add(1)
		workQueue.EnqueueRaw(claim, func(ctx context.Context, obj any) error {
			done, prepared := d.nodePrepareResource(ctx, claim)
			if done {
				preparedResources.Claims[claim.UID] = prepared
				wg.Done()
				return nil
			}
			return fmt.Errorf("%s", prepared.Error)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)

	return preparedResources, nil
}

func (d *driver) NodeUnprepareResources(ctx context.Context, req *drapbv1.NodeUnprepareResourcesRequest) (*drapbv1.NodeUnprepareResourcesResponse, error) {
	klog.Infof("NodeUnprepareResource is called: number of claims: %d", len(req.Claims))
	unpreparedResources := &drapbv1.NodeUnprepareResourcesResponse{Claims: map[string]*drapbv1.NodeUnprepareResourceResponse{}}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)
	workQueue := workqueue.New(workqueue.DefaultControllerRateLimiter())

	for _, claim := range req.Claims {
		wg.Add(1)
		workQueue.EnqueueRaw(claim, func(ctx context.Context, obj any) error {
			done, unprepared := d.nodeUnprepareResource(ctx, claim)
			if done {
				unpreparedResources.Claims[claim.UID] = unprepared
				wg.Done()
				return nil
			}
			return fmt.Errorf("%s", unprepared.Error)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)

	return unpreparedResources, nil
}

func (d *driver) nodePrepareResource(ctx context.Context, claim *drapbv1.Claim) (bool, *drapbv1.NodePrepareResourceResponse) {
	d.Lock()
	defer d.Unlock()

	resourceClaim, err := d.client.ResourceV1beta1().ResourceClaims(claim.Namespace).Get(
		ctx,
		claim.Name,
		metav1.GetOptions{})
	if err != nil {
		ret := &drapbv1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("failed to fetch ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return isPermanentError(err), ret
	}

	if resourceClaim.Status.Allocation == nil {
		ret := &drapbv1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("no allocation set in ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return true, ret
	}

	prepared, err := d.state.Prepare(ctx, resourceClaim)
	if err != nil {
		ret := &drapbv1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("error preparing devices for claim %v: %v", claim.UID, err),
		}
		return isPermanentError(err), ret
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, prepared)
	return true, &drapbv1.NodePrepareResourceResponse{Devices: prepared}
}

func (d *driver) nodeUnprepareResource(ctx context.Context, claim *drapbv1.Claim) (bool, *drapbv1.NodeUnprepareResourceResponse) {
	d.Lock()
	defer d.Unlock()

	resourceClaim, err := d.client.ResourceV1beta1().ResourceClaims(claim.Namespace).Get(
		ctx,
		claim.Name,
		metav1.GetOptions{})
	if err != nil {
		ret := &drapbv1.NodeUnprepareResourceResponse{
			Error: fmt.Sprintf("failed to fetch ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return isPermanentError(err), ret
	}

	if resourceClaim.Status.Allocation == nil {
		ret := &drapbv1.NodeUnprepareResourceResponse{
			Error: fmt.Sprintf("no allocation set in ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return true, ret
	}

	if err := d.state.Unprepare(ctx, resourceClaim); err != nil {
		ret := &drapbv1.NodeUnprepareResourceResponse{
			Error: fmt.Sprintf("error unpreparing devices for claim %v: %v", claim.UID, err),
		}
		return isPermanentError(err), ret
	}

	return true, &drapbv1.NodeUnprepareResourceResponse{}
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
