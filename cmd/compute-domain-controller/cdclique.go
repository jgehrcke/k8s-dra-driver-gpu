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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/nvidia.com/informers/externalversions"
)

// ComputeDomainCliqueManager manages ComputeDomainClique objects, providing
// Get/Update methods and periodic cleanup of stale entries.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory       nvinformers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	mutationCache cache.MutationCache
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager.
func NewComputeDomainCliqueManager(config *ManagerConfig) *ComputeDomainCliqueManager {
	factory := nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.driverNamespace),
	)
	informer := factory.Resource().V1beta1().ComputeDomainCliques().Informer()

	m := &ComputeDomainCliqueManager{
		config:   config,
		factory:  factory,
		informer: informer,
	}

	return m
}

// Start starts the ComputeDomainCliqueManager.
func (m *ComputeDomainCliqueManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainCliqueManager: %v", err)
			}
		}
	}()

	// Create mutation cache to track updates
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomainCliques failed")
	}

	// Start periodic cleanup
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.periodicCleanup(ctx)
	}()

	return nil
}

// Stop stops the ComputeDomainCliqueManager.
func (m *ComputeDomainCliqueManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// Get returns the ComputeDomainClique for the given ComputeDomain UID and clique ID.
// The clique name is "<computeDomainUID>.<cliqueID>".
func (m *ComputeDomainCliqueManager) Get(cdUID, cliqueID string) *nvapi.ComputeDomainClique {
	key := fmt.Sprintf("%s/%s.%s", m.config.driverNamespace, cdUID, cliqueID)
	obj, exists, err := m.mutationCache.GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	clique, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return nil
	}
	return clique
}

// Update updates a ComputeDomainClique and caches the result in the mutation cache.
func (m *ComputeDomainCliqueManager) Update(ctx context.Context, clique *nvapi.ComputeDomainClique) (*nvapi.ComputeDomainClique, error) {
	updatedClique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(clique.Namespace).Update(ctx, clique, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	m.mutationCache.Mutation(updatedClique)
	return updatedClique, nil
}

// RemoveDaemonInfo removes a daemon entry from the clique for the given
// ComputeDomain UID, clique ID, and node name.
func (m *ComputeDomainCliqueManager) RemoveDaemonInfo(ctx context.Context, cdUID, cliqueID, nodeName string) error {
	clique := m.Get(cdUID, cliqueID)
	if clique == nil {
		klog.V(4).Infof("No clique found for cdUID %s, cliqueID %s", cdUID, cliqueID)
		return nil
	}

	newClique := clique.DeepCopy()
	newClique.Daemons = filterOutDaemonByNodeName(newClique.Daemons, nodeName)
	if len(newClique.Daemons) == len(clique.Daemons) {
		klog.V(4).Infof("No daemon info found for node %s in clique %s/%s", nodeName, newClique.Namespace, newClique.Name)
		return nil
	}

	if _, err := m.Update(ctx, newClique); err != nil {
		return fmt.Errorf("error updating ComputeDomainClique %s/%s: %w", newClique.Namespace, newClique.Name, err)
	}

	klog.Infof("Successfully removed daemon info for node %s from ComputeDomainClique %s/%s", nodeName, newClique.Namespace, newClique.Name)
	return nil
}

// filterOutDaemonByNodeName filters out a daemon entry by node name.
func filterOutDaemonByNodeName(daemons []*nvapi.ComputeDomainDaemonInfo, nodeName string) []*nvapi.ComputeDomainDaemonInfo {
	var result []*nvapi.ComputeDomainDaemonInfo
	for _, d := range daemons {
		if d.NodeName != nodeName {
			result = append(result, d)
		}
	}
	return result
}

// periodicCleanup runs cleanup periodically.
func (m *ComputeDomainCliqueManager) periodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	// Run cleanup once at startup
	m.cleanup(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			klog.V(6).Infof("Running periodic cleanup for ComputeDomainClique entries")
			m.cleanup(ctx)
		}
	}
}

// cleanup reconciles clique entries against running pods.
func (m *ComputeDomainCliqueManager) cleanup(ctx context.Context) {
	// List all daemon pods in the driver namespace
	podList, err := m.config.clientsets.Core.CoreV1().Pods(m.config.driverNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: computeDomainLabelKey,
	})
	if err != nil {
		klog.Errorf("CliqueCleanup: error listing daemon pods: %v", err)
		return
	}

	// Build a set of node names that have running daemon pods
	activeNodes := make(map[string]struct{})
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			activeNodes[pod.Spec.NodeName] = struct{}{}
		}
	}

	// Get all cliques from the cache
	cliques := m.informer.GetStore().List()
	for _, obj := range cliques {
		clique, ok := obj.(*nvapi.ComputeDomainClique)
		if !ok {
			continue
		}
		m.cleanupClique(ctx, clique, activeNodes)
	}
}

// cleanupClique removes stale daemon entries from a single clique.
func (m *ComputeDomainCliqueManager) cleanupClique(ctx context.Context, clique *nvapi.ComputeDomainClique, activeNodes map[string]struct{}) {
	var updatedDaemons []*nvapi.ComputeDomainDaemonInfo
	var removedNodes []string

	for _, daemon := range clique.Daemons {
		if _, exists := activeNodes[daemon.NodeName]; exists {
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

	if _, err := m.Update(ctx, newClique); err != nil {
		klog.Errorf("CliqueCleanup: error updating ComputeDomainClique %s/%s: %v", clique.Namespace, clique.Name, err)
		return
	}

	klog.Infof("CliqueCleanup: successfully removed %d stale daemon entries from clique %s/%s", len(removedNodes), clique.Namespace, clique.Name)
}
