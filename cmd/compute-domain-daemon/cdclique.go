/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/nvidia.com/informers/externalversions"
)

// ComputeDomainCliqueManager watches ComputeDomainClique objects and updates them with
// info about the ComputeDomain daemon running on this node. This is an alternative
// to ComputeDomainStatusManager that works directly with CDClique objects instead
// of writing to ComputeDomain.Status.Nodes.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  nvinformers.SharedInformerFactory
	informer cache.SharedIndexInformer

	// Note: if `previousDaemons` is empty it means we're early in the daemon's
	// lifecycle and the IMEX daemon child process wasn't started yet.
	previousDaemons    []*nvapi.ComputeDomainDaemonInfo
	updatedDaemonsChan chan []*nvapi.ComputeDomainDaemonInfo

	podManager    *PodManager
	mutationCache cache.MutationCache
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager instance.
func NewComputeDomainCliqueManager(config *ManagerConfig) *ComputeDomainCliqueManager {
	m := &ComputeDomainCliqueManager{
		config:             config,
		previousDaemons:    []*nvapi.ComputeDomainDaemonInfo{},
		updatedDaemonsChan: make(chan []*nvapi.ComputeDomainDaemonInfo),
	}

	m.factory = nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.podNamespace), // CDClique is in the driver namespace
		nvinformers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.name=%s", m.cliqueName())
		}),
	)
	m.informer = m.factory.Resource().V1beta1().ComputeDomainCliques().Informer()

	return m
}

// Start starts the CDClique manager.
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

	// Create mutation cache to track our own updates
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	m.podManager = NewPodManager(m.config, m.updatePodReadiness)

	// Use `WithKey` with hard-coded key, to cancel any previous update task (we
	// want to make sure that the latest CDClique update wins).
	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.config.workQueue.EnqueueWithKey(obj, "cdclique", m.onAddOrUpdate)
		},
		UpdateFunc: func(objOld, objNew any) {
			m.config.workQueue.EnqueueWithKey(objNew, "cdclique", m.onAddOrUpdate)
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

	// Create the CDClique if it doesn't exist, or add our owner reference if it does.
	// The actual daemon info will be synced when the informer triggers onAddOrUpdate.
	if _, err := m.updateDaemonInfo(ctx, nil); err != nil {
		return fmt.Errorf("failed to create/update CDClique with owner reference: %w", err)
	}

	if err := m.podManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start pod manager: %w", err)
	}

	return nil
}

// Stop stops the CDClique manager.
//
//nolint:contextcheck
func (m *ComputeDomainCliqueManager) Stop() error {
	// Stop the pod manager first
	if m.podManager != nil {
		if err := m.podManager.Stop(); err != nil {
			klog.Errorf("Failed to stop pod manager: %v", err)
		}
	}

	// Create a new context for cleanup operations since the original context might be cancelled
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()

	// Attempt to remove this daemon from the CDClique before shutting down
	// Don't return error here as we still want to proceed with shutdown
	if err := m.removeDaemonInfoFromClique(cleanupCtx); err != nil {
		klog.Errorf("Failed to remove daemon info from CDClique during shutdown: %v", err)
	}

	if m.cancelContext != nil {
		m.cancelContext()
	}

	m.waitGroup.Wait()
	return nil
}

// GetDaemonInfoUpdateChan returns the channel that yields daemon info updates.
func (m *ComputeDomainCliqueManager) GetDaemonInfoUpdateChan() chan []*nvapi.ComputeDomainDaemonInfo {
	return m.updatedDaemonsChan
}

// cliqueName returns the name of the ComputeDomainClique object this daemon manages,
// constructed as "<computeDomainUUID>.<cliqueID>".
func (m *ComputeDomainCliqueManager) cliqueName() string {
	return fmt.Sprintf("%s.%s", m.config.computeDomainUUID, m.config.cliqueID)
}

// getClique gets the ComputeDomainClique from the mutation cache by namespace/name key.
func (m *ComputeDomainCliqueManager) getClique() (*nvapi.ComputeDomainClique, error) {
	key := fmt.Sprintf("%s/%s", m.config.podNamespace, m.cliqueName())
	obj, exists, err := m.mutationCache.GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("error retrieving ComputeDomainClique: %w", err)
	}
	if !exists {
		return nil, nil
	}
	clique, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return nil, fmt.Errorf("unexpected object type in cache")
	}
	return clique, nil
}

// onAddOrUpdate handles the addition or update of a ComputeDomainClique. Here, we
// receive updates not for all CDCliques in the system, but only for the one that we
// are registered for (filtered by name). Note that the informer triggers this
// callback once upon startup for all existing objects.
func (m *ComputeDomainCliqueManager) onAddOrUpdate(ctx context.Context, obj any) error {
	// Cast the object to a ComputeDomainClique object
	o, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return fmt.Errorf("failed to cast to ComputeDomainClique")
	}

	// Skip if this isn't our clique (should not happen with name filter, but be safe)
	if o.Name != m.cliqueName() {
		return nil
	}

	// Update daemon info in CDClique, if required.
	clique, err := m.syncDaemonInfoToClique(ctx)
	if err != nil {
		return fmt.Errorf("CDClique update: failed to insert/update daemon info: %w", err)
	}
	m.maybePushDaemonsUpdate(clique)

	return nil
}

// syncDaemonInfoToClique makes sure that the current daemon (by node name) is
// represented in the `Daemons` field in the CDClique object, and that it
// reports the IP address of this current pod running the CD daemon. If mutation
// is needed (first insertion, or IP address update) and successful, it reflects
// the mutation in `m.mutationCache`. Each daemon pod adds itself as an owner
// reference via SSA.
func (m *ComputeDomainCliqueManager) syncDaemonInfoToClique(ctx context.Context) (*nvapi.ComputeDomainClique, error) {
	clique, err := m.getClique()
	if err != nil {
		return nil, fmt.Errorf("failed to get CDClique: %w", err)
	}
	if clique == nil {
		return nil, fmt.Errorf("CDClique '%s' not found", m.cliqueName())
	}

	// Create a deep copy of the CDClique to avoid modifying the original
	newClique := clique.DeepCopy()

	// Try to find an existing entry for the current k8s node
	var myDaemon, myDaemonPrevious *nvapi.ComputeDomainDaemonInfo
	for _, d := range newClique.Daemons {
		if d.NodeName == m.config.nodeName {
			myDaemon = d.DeepCopy()
			myDaemonPrevious = d.DeepCopy()
			break
		}
	}

	// Create new ComputeDomainDaemonInfo object representing myself, and insert it into the daemons list.
	if myDaemon == nil {
		// Get the next available index for this new daemon (local point of view,
		// API server may tell us later that this index was chosen poorly).
		nextIndex, err := m.getNextAvailableIndex(newClique.Daemons)
		if err != nil {
			return nil, fmt.Errorf("error getting next available index: %w", err)
		}

		myDaemon = &nvapi.ComputeDomainDaemonInfo{
			NodeName: m.config.nodeName,
			CliqueID: m.config.cliqueID,
			Index:    nextIndex,
			// This is going to be switched to Ready by podmanager.
			Status: nvapi.ComputeDomainStatusNotReady,
		}

		klog.Infof("CDClique does not contain node name '%s' yet, try to insert myself: %v", m.config.nodeName, myDaemon)
	}

	// Unconditionally update its IP address. Note that the daemonInfo.IPAddress
	// as of now translates into a pod IP address and may therefore change
	// across pod restarts.
	myDaemon.IPAddress = m.config.podIP

	// Detect and handle DNS index collision where my self-chosen DNS index
	// appears elsewhere. If `m.previousDaemons` is empty, we haven't started
	// the IMEX daemon yet and hence can change our previous choice safely.
	if len(m.previousDaemons) == 0 {
		for _, other := range newClique.Daemons {
			// Skip myself
			if other.NodeName == m.config.nodeName {
				continue
			}
			if other.Index == myDaemon.Index {
				idx, err := m.getNextAvailableIndex(newClique.Daemons)
				if err != nil {
					return nil, fmt.Errorf("error getting next available index: %w", err)
				}
				myDaemon.Index = idx
				klog.V(4).Infof("syncDaemonInfoToClique: IMEX daemon not started yet, DNS index collision with %v, picked new index: %d", other, idx)
			}
		}
	}

	if myDaemonPrevious != nil && *myDaemonPrevious == *myDaemon {
		klog.V(6).Infof("syncDaemonInfoToClique noop: no change (%v)", *myDaemon)
		return newClique, nil
	}

	updatedClique, err := m.updateDaemonInfo(ctx, myDaemon)
	if err != nil {
		return nil, fmt.Errorf("error updating daemon info: %w", err)
	}

	klog.Infof("Successfully inserted/updated daemon info in CDClique %s (daemonInfo: %v)", m.cliqueName(), myDaemon)
	return updatedClique, nil
}

// The Index field in the Daemons section of the CDClique ensures a consistent
// IP-to-DNS name mapping across all machines within a given IMEX domain. Each
// daemon's index directly determines its DNS name using the format
// "compute-domain-daemon-{index}".
//
// getNextAvailableIndex finds the next available index for the current daemon by
// seeing which ones are already taken by other daemons in the CDClique. It fills
// in gaps where it can, and returns an error if no index is available within
// maxNodesPerIMEXDomain.
//
// By filling gaps in the index sequence (rather than always appending), we
// maintain stable DNS names for existing daemons even when intermediate daemons
// are removed from the clique and new ones are added.
func (m *ComputeDomainCliqueManager) getNextAvailableIndex(daemons []*nvapi.ComputeDomainDaemonInfo) (int, error) {
	// Create a map to track used indices
	usedIndices := make(map[int]bool)

	// Collect all currently used indices
	for _, d := range daemons {
		usedIndices[d.Index] = true
	}

	// Find the next available index, starting from 0 and filling gaps
	nextIndex := 0
	for usedIndices[nextIndex] {
		nextIndex++
	}

	// Ensure nextIndex is within the allowed range
	if nextIndex < 0 || nextIndex >= m.config.maxNodesPerIMEXDomain {
		return -1, fmt.Errorf("no available indices within maxNodesPerIMEXDomain (%d)", m.config.maxNodesPerIMEXDomain)
	}

	return nextIndex, nil
}

// removeDaemonInfoFromClique removes the current daemon info entry from the CDClique.
func (m *ComputeDomainCliqueManager) removeDaemonInfoFromClique(ctx context.Context) error {
	// Patch with nil: instructs to delete all list items owned by the SSA field
	// manager (which is precisely one: the item for this daemon info, as
	// identified by node name).
	if _, err := m.updateDaemonInfo(ctx, nil); err != nil {
		return fmt.Errorf("error removing daemon info from CDClique: %w", err)
	}

	klog.Infof("Successfully removed daemon info from CDClique %s", m.cliqueName())
	return nil
}

// If there was actually a change compared to the previously known set of
// daemons: pass info to IMEX daemon controller.
func (m *ComputeDomainCliqueManager) maybePushDaemonsUpdate(clique *nvapi.ComputeDomainClique) {
	// Do not update the IMEX daemon config if the current daemons list
	// contains any duplicate DNS index.
	if m.hasDuplicateIndex(clique.Daemons) {
		return
	}

	newIPs := m.getIPSetFromDaemons(clique.Daemons)
	previousIPs := m.getIPSetFromDaemons(m.previousDaemons)

	// Compare sets (i.e., without paying attention to order). Note: the order
	// of IP addresses written to the IMEX daemon's config file might matter (in
	// the sense that if across config files the set is equal but the order is
	// not: that may lead to an IMEX daemon startup error). Maybe we should
	// perform a stable sort of IP addresses before writing them to the nodes
	// config file.
	if !maps.Equal(newIPs, previousIPs) {
		klog.V(2).Infof("IP set changed")
		// This log message gets large for large node numbers
		klog.V(6).Infof("previous: %v; new: %v", previousIPs, newIPs)
		m.previousDaemons = clique.Daemons
		m.updatedDaemonsChan <- clique.Daemons
	} else {
		klog.V(6).Infof("IP set did not change")
	}
}

// updatePodReadiness updates the daemon info status based on pod readiness.
func (m *ComputeDomainCliqueManager) updatePodReadiness(ctx context.Context, ready bool) error {
	status := nvapi.ComputeDomainStatusNotReady
	if ready {
		status = nvapi.ComputeDomainStatusReady
	}

	clique, err := m.getClique()
	if err != nil {
		return fmt.Errorf("failed to get CDClique: %w", err)
	}
	if clique == nil {
		return fmt.Errorf("CDClique '%s' not found", m.cliqueName())
	}

	// Find the daemon info
	var myDaemon *nvapi.ComputeDomainDaemonInfo
	for _, d := range clique.Daemons {
		if d.NodeName == m.config.nodeName {
			myDaemon = d.DeepCopy()
			break
		}
	}

	// If daemon info not found, exit early and retry
	if myDaemon == nil {
		return fmt.Errorf("daemon info not yet listed in CDClique (waiting for insertion)")
	}

	// If status hasn't changed, exit early
	if myDaemon.Status == status {
		klog.V(6).Infof("updatePodReadiness noop: status not changed (%s)", status)
		return nil
	}

	// Update the daemon info status
	myDaemon.Status = status

	if _, err := m.updateDaemonInfo(ctx, myDaemon); err != nil {
		return fmt.Errorf("error updating daemon info status: %w", err)
	}

	klog.Infof("Successfully updated daemon info status in CDClique (new status: %s)", status)
	return nil
}

// updateDaemonInfo patches the CDClique with the provided daemon info and updates
// the mutation cache. Pass nil to remove the daemon info.
func (m *ComputeDomainCliqueManager) updateDaemonInfo(ctx context.Context, daemon *nvapi.ComputeDomainDaemonInfo) (*nvapi.ComputeDomainClique, error) {
	var daemons []*nvapi.ComputeDomainDaemonInfo
	if daemon != nil {
		daemons = []*nvapi.ComputeDomainDaemonInfo{daemon}
	}

	patchBytes, err := m.generatePatchForDaemonInfo(daemons)
	if err != nil {
		return nil, fmt.Errorf("could not serialize patch: %w", err)
	}

	updatedClique, err := m.patchClique(ctx, patchBytes)
	if err != nil {
		return nil, fmt.Errorf("error patching CDClique: %w", err)
	}
	m.mutationCache.Mutation(updatedClique)

	return updatedClique, nil
}

func (m *ComputeDomainCliqueManager) getIPSetFromDaemons(daemons []*nvapi.ComputeDomainDaemonInfo) IPSet {
	set := make(IPSet)
	for _, d := range daemons {
		set[d.IPAddress] = struct{}{}
	}
	return set
}

// hasDuplicateIndex iterates over the list of ComputeDomainDaemonInfos and
// returns true if any Index appears more than once.
func (m *ComputeDomainCliqueManager) hasDuplicateIndex(daemons []*nvapi.ComputeDomainDaemonInfo) bool {
	seen := make(map[int]struct{})

	for _, daemon := range daemons {
		if _, exists := seen[daemon.Index]; exists {
			klog.V(4).Infof("DNS index collision detected: %v uses an index seen before (we are node %v)", daemon, m.config.nodeName)
			return true
		}

		// Mark as seen.
		seen[daemon.Index] = struct{}{}
	}

	return false
}

// Notes:
//
// 1) Field owner and field manager are referring to the same concept.
// Specifically, `client.FieldOwner("foo")` (which is often shown in
// documentation snippets) renders as `PatchOptions{FieldManager: "foo"}`.
//
// 2) In SSA documentation, one finds the `force: true` concept -- it can be
// used to take ownership of fields that are currently owned by a different
// field manager. This is not needed in our context.
//
// 3) We cannot get away with one shared owner/manager that is used across
// nodes. SSA tracks field ownership at the field manager level, not at the
// client/process level. When a field manager applies a patch with a list, it's
// declaring the complete desired state for all entries that this field manager
// owns. When multiple writers use the same field manager name, all such writes
// are treated as coming from one logical actor. Notably, each apply operation
// must include all fields that field manager owns. If daemon A adds/updates entry
// "daemon-a" and daemon B adds/updates entry "daemon-b" using the same field manager,
// SSA interprets the omission as intent to delete those entries.
//
// Note:
//   - The `Patch()` method requires the patch itself to be provided as
//     byte sequence (as JSON document).
//   - The `apiVersion` and `kind" fields are required in the patch payload.
//   - We verified that the API server object returned in response to a PATCH
//     request reflects both, the patch, and also patches made by other
//     owners if they happened in the meantime.
func (m *ComputeDomainCliqueManager) patchClique(ctx context.Context, patch []byte) (*nvapi.ComputeDomainClique, error) {
	updatedClique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.podNamespace).Patch(
		ctx,
		m.cliqueName(),
		types.ApplyPatchType,
		patch,
		metav1.PatchOptions{
			FieldManager: fmt.Sprintf("cd-daemon-%s", m.config.nodeName),
		},
	)
	return updatedClique, err
}

func (m *ComputeDomainCliqueManager) generatePatchForDaemonInfo(daemons []*nvapi.ComputeDomainDaemonInfo) ([]byte, error) {
	patch := map[string]interface{}{
		"apiVersion": nvapi.GroupName + "/" + nvapi.Version,
		"kind":       nvapi.ComputeDomainCliqueKind,
		"metadata": map[string]interface{}{
			"name":      m.cliqueName(),
			"namespace": m.config.podNamespace,
			"labels": map[string]string{
				computeDomainLabelKey:       m.config.computeDomainUUID,
				computeDomainCliqueLabelKey: m.config.cliqueID,
			},
			"ownerReferences": []map[string]interface{}{
				{
					"apiVersion": "v1",
					"kind":       "Pod",
					"name":       m.config.podName,
					"uid":        m.config.podUID,
				},
			},
		},
	}
	if daemons != nil {
		patch["daemons"] = daemons
	}
	patchBytes, err := json.Marshal(patch)
	return patchBytes, err
}
