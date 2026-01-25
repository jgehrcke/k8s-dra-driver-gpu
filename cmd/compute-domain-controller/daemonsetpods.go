/*
 * Copyright (c) 2025 NVIDIA CORPORATION.  All rights reserved.
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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
)

type DaemonSetPodManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  informers.SharedInformerFactory
	informer cache.SharedIndexInformer
	lister   corev1listers.PodLister

	getComputeDomain          GetComputeDomainFunc
	updateComputeDomainStatus UpdateComputeDomainStatusFunc
	cliqueManager             *ComputeDomainCliqueManager
}

func NewDaemonSetPodManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *DaemonSetPodManager {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Core,
		informerResyncPeriod,
		informers.WithNamespace(config.driverNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = metav1.FormatLabelSelector(labelSelector)
		}),
	)

	informer := factory.Core().V1().Pods().Informer()
	lister := factory.Core().V1().Pods().Lister()

	m := &DaemonSetPodManager{
		config:                    config,
		factory:                   factory,
		informer:                  informer,
		lister:                    lister,
		getComputeDomain:          getComputeDomain,
		updateComputeDomainStatus: updateComputeDomainStatus,
	}

	if featuregates.Enabled(featuregates.ComputeDomainCliques) {
		m.cliqueManager = NewComputeDomainCliqueManager(config, getComputeDomain, updateComputeDomainStatus)
	}

	return m
}

func (m *DaemonSetPodManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping DaemonSetPod manager: %v", err)
			}
		}
	}()

	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			m.config.workQueue.Enqueue(obj, m.onPodDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for pod informer: %w", err)
	}

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("error syncing pod informer: %w", err)
	}

	if m.cliqueManager != nil {
		if err := m.cliqueManager.Start(ctx); err != nil {
			return fmt.Errorf("error starting ComputeDomainClique manager: %w", err)
		}
	}

	return nil
}

func (m *DaemonSetPodManager) Stop() error {
	if m.cliqueManager != nil {
		if err := m.cliqueManager.Stop(); err != nil {
			klog.Errorf("error stopping ComputeDomainClique manager: %v", err)
		}
	}
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *DaemonSetPodManager) onPodDelete(ctx context.Context, obj any) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("failed to cast to Pod")
	}

	cd, err := m.getComputeDomain(p.Labels[computeDomainLabelKey])
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return nil
	}

	// Use different cleanup logic based on feature gate
	if m.cliqueManager != nil {
		if err := m.removeDaemonInfoFromClique(ctx, p, cd); err != nil {
			return fmt.Errorf("error removing daemon info from clique: %w", err)
		}
	} else {
		if err := m.removeNodeFromComputeDomainStatus(ctx, p, cd); err != nil {
			return fmt.Errorf("error removing node from ComputeDomain status: %w", err)
		}
	}

	return nil
}

// removeNodeFromComputeDomainStatus removes a node from ComputeDomain.Status.Nodes
// based on the pod's IP address.
func (m *DaemonSetPodManager) removeNodeFromComputeDomainStatus(ctx context.Context, p *corev1.Pod, cd *nvapi.ComputeDomain) error {
	newCD := cd.DeepCopy()

	// Filter out the node with the current pod's IP address
	var updatedNodes []*nvapi.ComputeDomainNode
	for _, node := range newCD.Status.Nodes {
		if node.IPAddress != p.Status.PodIP {
			updatedNodes = append(updatedNodes, node)
		}
	}

	// If no nodes were removed, nothing to do
	if len(updatedNodes) == len(cd.Status.Nodes) {
		return nil
	}

	// Update the ComputeDomain status with the new list of nodes
	newCD.Status.Nodes = updatedNodes
	if _, err := m.updateComputeDomainStatus(ctx, newCD); err != nil {
		return fmt.Errorf("error updating ComputeDomain status: %w", err)
	}

	klog.Infof("Successfully removed node with IP %s from ComputeDomain %s/%s", p.Status.PodIP, newCD.Namespace, newCD.Name)
	return nil
}

// removeDaemonInfoFromClique removes daemon info from the ComputeDomainClique
// when a daemon pod is deleted.
func (m *DaemonSetPodManager) removeDaemonInfoFromClique(ctx context.Context, p *corev1.Pod, cd *nvapi.ComputeDomain) error {
	nodeName := p.Spec.NodeName
	if nodeName == "" {
		klog.V(4).Infof("Pod %s/%s has no nodeName, skipping clique cleanup", p.Namespace, p.Name)
		return nil
	}

	cliqueID := p.Labels[computeDomainCliqueLabelKey]
	if cliqueID == "" {
		klog.V(4).Infof("Pod %s/%s has no cliqueID label, skipping clique cleanup", p.Namespace, p.Name)
		return nil
	}

	err := m.cliqueManager.RemoveDaemonInfo(ctx, string(cd.UID), cliqueID, nodeName)
	if err != nil {
		return fmt.Errorf("error removing daemon info from clique: %w", err)
	}
	return nil
}
