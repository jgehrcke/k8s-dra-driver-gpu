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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
)

const (
	// cliqueCleanupInterval is how often to run the periodic cleanup
	// that removes stale daemon entries from cliques.
	cliqueCleanupInterval = 5 * time.Minute
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

func NewDaemonSetPodManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *DaemonSetPodManager {
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
		m.cliqueManager = NewComputeDomainCliqueManager(config, getComputeDomain, listComputeDomains, updateComputeDomainStatus)
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

		// Run cleanup once at startup
		m.cleanup(ctx)

		// Start periodic cleanup loop
		m.waitGroup.Add(1)
		go func() {
			defer m.waitGroup.Done()
			m.startPeriodicCleanup(ctx)
		}()
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

// List returns all daemon pods from the informer cache.
func (m *DaemonSetPodManager) List() ([]*corev1.Pod, error) {
	return m.lister.Pods(m.config.driverNamespace).List(labels.Everything())
}

// startPeriodicCleanup runs the cleanup every cliqueCleanupInterval.
func (m *DaemonSetPodManager) startPeriodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(cliqueCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			klog.V(6).Infof("Running periodic cleanup for orphaned daemon entries in cliques")
			m.cleanup(ctx)
		}
	}
}

// cleanup removes daemon entries from cliques for pods that no longer exist.
func (m *DaemonSetPodManager) cleanup(ctx context.Context) {
	pods, err := m.List()
	if err != nil {
		klog.Errorf("CliqueCleanup: error listing pods: %v", err)
		return
	}

	// Build set of node names that have running daemon pods
	daemons := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			daemons[pod.Spec.NodeName] = struct{}{}
		}
	}

	cliques, err := m.cliqueManager.List()
	if err != nil {
		klog.Errorf("CliqueCleanup: error listing cliques: %v", err)
		return
	}

	for _, clique := range cliques {
		m.cleanupClique(ctx, clique, daemons)
	}
}

// cleanupClique removes stale daemon entries from a single clique.
func (m *DaemonSetPodManager) cleanupClique(ctx context.Context, clique *nvapi.ComputeDomainClique, daemons map[string]struct{}) {
	var updatedDaemons []*nvapi.ComputeDomainDaemonInfo
	var removedNodes []string

	for _, daemon := range clique.Daemons {
		if _, exists := daemons[daemon.NodeName]; exists {
			updatedDaemons = append(updatedDaemons, daemon)
		} else {
			removedNodes = append(removedNodes, daemon.NodeName)
		}
	}

	// Nothing to clean up
	if len(removedNodes) == 0 {
		return
	}

	klog.Infof("CliqueCleanup: removing stale daemon entries from clique %s/%s: %v", clique.Namespace, clique.Name, removedNodes)

	// Update the clique with the filtered daemon list
	newClique := clique.DeepCopy()
	newClique.Daemons = updatedDaemons

	if _, err := m.cliqueManager.Update(ctx, newClique); err != nil {
		klog.Errorf("CliqueCleanup: error updating ComputeDomainClique %s/%s: %v", clique.Namespace, clique.Name, err)
		return
	}

	klog.Infof("CliqueCleanup: successfully removed %d stale daemon entries from clique %s/%s", len(removedNodes), clique.Namespace, clique.Name)
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

	// Always remove from status
	if err := m.removeNodeFromComputeDomainStatus(ctx, p, cd); err != nil {
		return fmt.Errorf("error removing node from ComputeDomain status: %w", err)
	}

	// Additionally remove from clique if feature gate is enabled
	if m.cliqueManager != nil {
		if err := m.removeDaemonInfoFromClique(ctx, p, cd); err != nil {
			return fmt.Errorf("error removing daemon info from clique: %w", err)
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
