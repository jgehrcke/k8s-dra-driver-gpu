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
	"maps"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/nvidia.com/informers/externalversions"
	nvlisters "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/nvidia.com/listers/resource/v1beta1"
)

const (
	// cdStatusSyncInterval is how often to sync clique info to CD status.
	cdStatusSyncInterval = 2 * time.Second
)

// ComputeDomainCliqueManager manages ComputeDomainClique objects, providing
// Get/List/Update methods for clique access and periodic sync to CD status.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory       nvinformers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	lister        nvlisters.ComputeDomainCliqueLister
	mutationCache cache.MutationCache

	listComputeDomains        ListComputeDomainsFunc
	updateComputeDomainStatus UpdateComputeDomainStatusFunc
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager.
func NewComputeDomainCliqueManager(config *ManagerConfig, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *ComputeDomainCliqueManager {
	factory := nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.driverNamespace),
	)
	informer := factory.Resource().V1beta1().ComputeDomainCliques().Informer()
	lister := nvlisters.NewComputeDomainCliqueLister(informer.GetIndexer())

	m := &ComputeDomainCliqueManager{
		config:                    config,
		factory:                   factory,
		informer:                  informer,
		lister:                    lister,
		listComputeDomains:        listComputeDomains,
		updateComputeDomainStatus: updateComputeDomainStatus,
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

	// Run sync once at startup
	klog.Info("CDStatusSync: running initial sync")
	m.syncCDStatus(ctx)

	// Start periodic sync loop
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.startPeriodicSync(ctx)
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

// List returns all ComputeDomainCliques from the informer cache.
func (m *ComputeDomainCliqueManager) List() ([]*nvapi.ComputeDomainClique, error) {
	return m.lister.ComputeDomainCliques(m.config.driverNamespace).List(labels.Everything())
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

// startPeriodicSync runs the sync every cdStatusSyncInterval.
func (m *ComputeDomainCliqueManager) startPeriodicSync(ctx context.Context) {
	ticker := time.NewTicker(cdStatusSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.syncCDStatus(ctx)
		}
	}
}

// syncCDStatus synchronizes clique information to all ComputeDomain statuses.
func (m *ComputeDomainCliqueManager) syncCDStatus(ctx context.Context) {
	cds, err := m.listComputeDomains()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing ComputeDomains: %v", err)
		return
	}

	cliques, err := m.List()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing cliques: %v", err)
		return
	}

	// Group cliques by CD UID
	cliquesByCD := make(map[string][]*nvapi.ComputeDomainClique)
	for _, clique := range cliques {
		cdUID := clique.Labels[computeDomainLabelKey]
		if cdUID == "" {
			continue
		}
		cliquesByCD[cdUID] = append(cliquesByCD[cdUID], clique)
	}

	// Sync each CD
	for _, cd := range cds {
		m.syncCD(ctx, cd, cliquesByCD[string(cd.UID)])
	}
}

// syncCD synchronizes clique information to a single ComputeDomain's status.
func (m *ComputeDomainCliqueManager) syncCD(ctx context.Context, cd *nvapi.ComputeDomain, cliques []*nvapi.ComputeDomainClique) {
	// Build the expected nodes list from cliques
	newNodes := m.buildNodesFromCliques(cliques)

	// Check if update is needed
	if nodesEqual(cd.Status.Nodes, newNodes) {
		return
	}

	klog.V(6).Infof("CDStatusSync: syncing ComputeDomain %s/%s", cd.Namespace, cd.Name)

	// Update status
	newCD := cd.DeepCopy()
	newCD.Status.Nodes = newNodes
	if _, err := m.updateComputeDomainStatus(ctx, newCD); err != nil {
		klog.Errorf("CDStatusSync: error updating ComputeDomain %s status: %v", cd.Name, err)
		return
	}

	klog.V(4).Infof("CDStatusSync: updated nodes in ComputeDomain %s/%s: nodes=%d", cd.Namespace, cd.Name, len(newNodes))
}

// buildNodesFromCliques builds a nodes list from all cliques.
func (m *ComputeDomainCliqueManager) buildNodesFromCliques(cliques []*nvapi.ComputeDomainClique) []*nvapi.ComputeDomainNode {
	var result []*nvapi.ComputeDomainNode
	for _, clique := range cliques {
		for _, daemon := range clique.Daemons {
			result = append(result, &nvapi.ComputeDomainNode{
				Name:      daemon.NodeName,
				IPAddress: daemon.IPAddress,
				CliqueID:  daemon.CliqueID,
				Index:     daemon.Index,
				Status:    daemon.Status,
			})
		}
	}
	return result
}

// nodesEqual checks if two slices of ComputeDomainNode are equal.
func nodesEqual(a, b []*nvapi.ComputeDomainNode) bool {
	aMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range a {
		aMap[node.Name] = *node
	}
	bMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range b {
		bMap[node.Name] = *node
	}
	return maps.Equal(aMap, bMap)
}
