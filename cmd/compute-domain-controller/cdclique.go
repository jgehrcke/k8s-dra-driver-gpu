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

// ComputeDomainCliqueManager manages ComputeDomainClique objects, providing
// Get/Update methods and periodic cleanup of stale entries.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory       nvinformers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	lister        nvlisters.ComputeDomainCliqueLister
	mutationCache cache.MutationCache

	getComputeDomain          GetComputeDomainFunc
	listComputeDomains        ListComputeDomainsFunc
	updateComputeDomainStatus UpdateComputeDomainStatusFunc
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager.
func NewComputeDomainCliqueManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *ComputeDomainCliqueManager {
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
		getComputeDomain:          getComputeDomain,
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

	// Add event handlers to update CD status when cliques change
	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.config.workQueue.Enqueue(obj, m.onAddOrUpdate)
		},
		UpdateFunc: func(oldObj, newObj any) {
			m.config.workQueue.Enqueue(newObj, m.onAddOrUpdate)
		},
		DeleteFunc: func(obj any) {
			m.config.workQueue.Enqueue(obj, m.onDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for ComputeDomainClique informer: %w", err)
	}

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

// onAddOrUpdate handles clique add/update events by updating the CD's nodes list.
func (m *ComputeDomainCliqueManager) onAddOrUpdate(ctx context.Context, obj any) error {
	clique, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return fmt.Errorf("failed to cast to ComputeDomainClique")
	}

	cdUID := clique.Labels[computeDomainLabelKey]
	if cdUID == "" {
		return nil
	}

	cliqueID := clique.Labels[computeDomainCliqueLabelKey]
	if cliqueID == "" {
		return nil
	}

	// Re-pull from cache to get the latest version
	clique = m.Get(cdUID, cliqueID)
	if clique == nil {
		return nil
	}

	return m.updateNodesInCDStatus(ctx, cdUID, cliqueID, clique)
}

// onDelete handles clique delete events by removing nodes from the CD's nodes list.
func (m *ComputeDomainCliqueManager) onDelete(ctx context.Context, obj any) error {
	clique, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return fmt.Errorf("failed to cast to ComputeDomainClique")
	}

	cdUID := clique.Labels[computeDomainLabelKey]
	if cdUID == "" {
		return nil
	}

	cliqueID := clique.Labels[computeDomainCliqueLabelKey]
	if cliqueID == "" {
		return nil
	}

	return m.updateNodesInCDStatus(ctx, cdUID, cliqueID, nil)
}

// updateNodesInCDStatus updates the CD's Status.Nodes for a specific clique.
// If clique is nil, nodes from that clique are removed.
func (m *ComputeDomainCliqueManager) updateNodesInCDStatus(ctx context.Context, cdUID, cliqueID string, clique *nvapi.ComputeDomainClique) error {
	cd, err := m.getComputeDomain(cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return nil
	}

	newNodes := m.buildUpdatedNodes(cd, cliqueID, clique)
	if nodesEqual(cd.Status.Nodes, newNodes) {
		return nil
	}

	newCD := cd.DeepCopy()
	newCD.Status.Nodes = newNodes
	if _, err := m.updateComputeDomainStatus(ctx, newCD); err != nil {
		return fmt.Errorf("error updating ComputeDomain status: %w", err)
	}

	klog.V(4).Infof("Updated nodes in ComputeDomain %s/%s: nodes=%d", cd.Namespace, cd.Name, len(newNodes))
	return nil
}

// buildUpdatedNodes builds an updated nodes list with nodes from the given clique added/updated/removed.
func (m *ComputeDomainCliqueManager) buildUpdatedNodes(cd *nvapi.ComputeDomain, cliqueID string, clique *nvapi.ComputeDomainClique) []*nvapi.ComputeDomainNode {
	var result []*nvapi.ComputeDomainNode

	// Keep nodes from other cliques
	for _, node := range cd.Status.Nodes {
		if node.CliqueID != cliqueID {
			result = append(result, node)
		}
	}

	// Return early if this is a delete
	if clique == nil {
		return result
	}

	// Add nodes from this clique
	for _, daemon := range clique.Daemons {
		result = append(result, &nvapi.ComputeDomainNode{
			Name:      daemon.NodeName,
			IPAddress: daemon.IPAddress,
			CliqueID:  daemon.CliqueID,
			Index:     daemon.Index,
			Status:    daemon.Status,
		})
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

// getDaemonsByCD returns a set of node names that have running daemon pods.
func (m *ComputeDomainCliqueManager) getDaemonsByCD(ctx context.Context) (map[string]struct{}, error) {
	podList, err := m.config.clientsets.Core.CoreV1().Pods(m.config.driverNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: computeDomainLabelKey,
	})
	if err != nil {
		return nil, err
	}

	daemons := make(map[string]struct{})
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			daemons[pod.Spec.NodeName] = struct{}{}
		}
	}
	return daemons, nil
}

// getCliquesByCD returns a map of cliques grouped by ComputeDomain UID.
func (m *ComputeDomainCliqueManager) getCliquesByCD() (map[string]map[string]*nvapi.ComputeDomainClique, error) {
	cliqueList, err := m.List()
	if err != nil {
		return nil, err
	}

	cliques := make(map[string]map[string]*nvapi.ComputeDomainClique)
	for _, clique := range cliqueList {
		cdUID := clique.Labels[computeDomainLabelKey]
		cliqueID := clique.Labels[computeDomainCliqueLabelKey]
		if cdUID == "" || cliqueID == "" {
			continue
		}

		if cliques[cdUID] == nil {
			cliques[cdUID] = make(map[string]*nvapi.ComputeDomainClique)
		}
		cliques[cdUID][cliqueID] = clique
	}
	return cliques, nil
}

// getNodesByCD returns a map of clique IDs that have nodes in each CD's Status.Nodes.
func (m *ComputeDomainCliqueManager) getNodesByCD() (map[string]map[string]struct{}, error) {
	cds, err := m.listComputeDomains()
	if err != nil {
		return nil, err
	}

	nodes := make(map[string]map[string]struct{})
	for _, cd := range cds {
		cdUID := string(cd.UID)
		for _, node := range cd.Status.Nodes {
			if nodes[cdUID] == nil {
				nodes[cdUID] = make(map[string]struct{})
			}
			nodes[cdUID][node.CliqueID] = struct{}{}
		}
	}
	return nodes, nil
}

// cleanup reconciles clique entries against running pods and removes stale CD status entries.
func (m *ComputeDomainCliqueManager) cleanup(ctx context.Context) {
	cliques, err := m.getCliquesByCD()
	if err != nil {
		klog.Errorf("CliqueCleanup: error getting cliques: %v", err)
		return
	}

	daemons, err := m.getDaemonsByCD(ctx)
	if err != nil {
		klog.Errorf("CliqueCleanup: error getting daemons: %v", err)
		return
	}

	nodes, err := m.getNodesByCD()
	if err != nil {
		klog.Errorf("CliqueCleanup: error getting nodes: %v", err)
		return
	}

	m.cleanupOrphanedDaemonsInCliques(ctx, daemons, cliques)
	m.cleanupOrphanedCliquesInNodes(ctx, cliques, nodes)
}

// cleanupOrphanedDaemonsInCliques removes stale daemon entries from cliques.
func (m *ComputeDomainCliqueManager) cleanupOrphanedDaemonsInCliques(ctx context.Context, daemons map[string]struct{}, cliques map[string]map[string]*nvapi.ComputeDomainClique) {
	for _, cliquesForCD := range cliques {
		for _, clique := range cliquesForCD {
			m.cleanupClique(ctx, clique, daemons)
		}
	}
}

// cleanupOrphanedCliquesInNodes removes Status.Nodes entries whose cliques no longer exist.
func (m *ComputeDomainCliqueManager) cleanupOrphanedCliquesInNodes(ctx context.Context, cliques map[string]map[string]*nvapi.ComputeDomainClique, nodes map[string]map[string]struct{}) {
	for cdUID, cliqueIDs := range nodes {
		for cliqueID := range cliqueIDs {
			if _, exists := cliques[cdUID][cliqueID]; exists {
				continue
			}
			if err := m.updateNodesInCDStatus(ctx, cdUID, cliqueID, nil); err != nil {
				klog.Errorf("CliqueCleanup: error removing orphaned nodes for clique %s: %v", cliqueID, err)
			}
		}
	}
}

// cleanupClique removes stale daemon entries from a single clique.
func (m *ComputeDomainCliqueManager) cleanupClique(ctx context.Context, clique *nvapi.ComputeDomainClique, daemons map[string]struct{}) {
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

	if _, err := m.Update(ctx, newClique); err != nil {
		klog.Errorf("CliqueCleanup: error updating ComputeDomainClique %s/%s: %v", clique.Namespace, clique.Name, err)
		return
	}

	klog.Infof("CliqueCleanup: successfully removed %d stale daemon entries from clique %s/%s", len(removedNodes), clique.Namespace, clique.Name)
}
