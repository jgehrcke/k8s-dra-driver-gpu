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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	nvapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/nvidia.com/informers/externalversions"
)

const (
	computeDomainLabelKey = "resource.nvidia.com/computeDomain"

	informerResyncPeriod = 10 * time.Minute
	cleanupInterval      = 10 * time.Minute

	ComputeDomainDaemonSettingsRoot       = DriverPluginPath + "/domains"
	ComputeDomainDaemonConfigTemplatePath = "/templates/compute-domain-daemon-config.tmpl.cfg"
)

type ComputeDomainManager struct {
	config        *Config
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  nvinformers.SharedInformerFactory
	informer cache.SharedIndexInformer

	configFilesRoot string
	cliqueID        string
}

type ComputeDomainDaemonSettings struct {
	manager         *ComputeDomainManager
	domain          string
	rootDir         string
	configPath      string
	nodesConfigPath string
}

func NewComputeDomainManager(config *Config, configFilesRoot, cliqueID string) *ComputeDomainManager {
	factory := nvinformers.NewSharedInformerFactory(config.clientsets.Nvidia, informerResyncPeriod)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()

	m := &ComputeDomainManager{
		config:          config,
		factory:         factory,
		informer:        informer,
		configFilesRoot: configFilesRoot,
		cliqueID:        cliqueID,
	}

	return m
}

func (m *ComputeDomainManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainManager: %v", err)
			}
		}
	}()

	err := m.informer.AddIndexers(cache.Indexers{
		"computeDomainUID": uidIndexer[*nvapi.ComputeDomain],
	})
	if err != nil {
		return fmt.Errorf("error adding indexer for UIDs: %w", err)
	}

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.periodicCleanup(ctx)
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomains failed")
	}

	return nil
}

func (m *ComputeDomainManager) Stop() error {
	m.cancelContext()
	m.waitGroup.Wait()
	return nil
}

func (m *ComputeDomainManager) NewSettings(domain string) *ComputeDomainDaemonSettings {
	return &ComputeDomainDaemonSettings{
		manager:         m,
		domain:          domain,
		rootDir:         fmt.Sprintf("%s/%s", m.configFilesRoot, domain),
		configPath:      fmt.Sprintf("%s/%s/%s", m.configFilesRoot, domain, "config.cfg"),
		nodesConfigPath: fmt.Sprintf("%s/%s/%s", m.configFilesRoot, domain, "nodes_config.cfg"),
	}
}

func (m *ComputeDomainManager) GetComputeDomainChannelContainerEdits(devRoot string, info *ComputeDomainChannelInfo) *cdiapi.ContainerEdits {
	channelPath := fmt.Sprintf("/dev/nvidia-caps-imex-channels/channel%d", info.ID)

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{
				{
					Path:     channelPath,
					HostPath: filepath.Join(devRoot, channelPath),
				},
			},
		},
	}
}

func (s *ComputeDomainDaemonSettings) GetDomain() string {
	return s.domain
}

func (s *ComputeDomainDaemonSettings) GetCDIContainerEdits(devRoot string, info *nvcapDeviceInfo) *cdiapi.ContainerEdits {

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: "/etc/nvidia-imex",
					HostPath:      s.rootDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
			DeviceNodes: []*cdispec.DeviceNode{
				{
					Path:     info.path,
					HostPath: filepath.Join(devRoot, info.path),
				},
			},
		},
	}
}

func (s *ComputeDomainDaemonSettings) Prepare(ctx context.Context) error {
	if err := os.MkdirAll(s.rootDir, 0755); err != nil {
		return fmt.Errorf("error creating directory %v: %w", s.rootDir, err)
	}

	if err := s.WriteConfigFile(ctx); err != nil {
		return fmt.Errorf("error writing config file %v: %w", s.configPath, err)
	}

	if err := s.WriteNodesConfigFile(ctx); err != nil {
		return fmt.Errorf("error writing nodes config file %v: %w", s.nodesConfigPath, err)
	}

	return nil
}

func (s *ComputeDomainDaemonSettings) Unprepare(ctx context.Context) error {
	if err := os.RemoveAll(s.rootDir); err != nil {
		return fmt.Errorf("error removing directory %v: %w", s.rootDir, err)
	}
	return nil
}

func (s *ComputeDomainDaemonSettings) WriteConfigFile(ctx context.Context) error {
	configTemplateData := struct{}{}

	tmpl, err := template.ParseFiles(ComputeDomainDaemonConfigTemplatePath)
	if err != nil {
		return fmt.Errorf("error parsing template file: %w", err)
	}

	var configFile bytes.Buffer
	if err := tmpl.Execute(&configFile, configTemplateData); err != nil {
		return fmt.Errorf("error executing template: %w", err)
	}

	if err := os.WriteFile(s.configPath, configFile.Bytes(), 0644); err != nil {
		return fmt.Errorf("error writing config file %v: %w", s.configPath, err)
	}

	return nil
}

func (s *ComputeDomainDaemonSettings) WriteNodesConfigFile(ctx context.Context) error {
	nodeIPs, err := s.manager.GetNodeIPs(ctx, s.domain)
	if err != nil {
		return fmt.Errorf("error getting node IPs: %w", err)
	}

	var nodesConfigFile bytes.Buffer
	for _, ip := range nodeIPs {
		nodesConfigFile.WriteString(fmt.Sprintf("%s\n", ip))
	}

	if err := os.WriteFile(s.nodesConfigPath, nodesConfigFile.Bytes(), 0644); err != nil {
		return fmt.Errorf("error writing config file %v: %w", s.configPath, err)
	}

	return nil
}

func (m *ComputeDomainManager) AssertComputeDomainReady(ctx context.Context, cdUID string) error {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	if cd.Status.Status != nvapi.ComputeDomainStatusReady {
		return fmt.Errorf("ComputeDomain not Ready")
	}

	return nil
}

func (m *ComputeDomainManager) AddNodeStatusToComputeDomain(ctx context.Context, cdUID string) error {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	if cd.Status.Status == nvapi.ComputeDomainStatusReady {
		return nil
	}

	var nodeNames []string
	for _, node := range cd.Status.Nodes {
		nodeNames = append(nodeNames, node.Name)
	}

	if slices.Contains(nodeNames, m.config.flags.nodeName) {
		return nil
	}

	node, err := m.GetComputeDomainNodeStatusInfo(ctx, m.config.flags.nodeName)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain node status info: %w", err)
	}

	newCD := cd.DeepCopy()
	newCD.Status.Nodes = append(newCD.Status.Nodes, node)
	newCD.Status.Status = nvapi.ComputeDomainStatusNotReady
	if _, err = m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomains(newCD.Namespace).UpdateStatus(ctx, newCD, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating nodes in ComputeDomain status: %w", err)
	}

	return nil
}

func (m *ComputeDomainManager) GetComputeDomainNodeStatusInfo(ctx context.Context, nodeName string) (*nvapi.ComputeDomainNode, error) {
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting Node '%s': %w", nodeName, err)
	}

	var ipAddress string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ipAddress = addr.Address
			break
		}
	}

	n := &nvapi.ComputeDomainNode{
		Name:      nodeName,
		IPAddress: ipAddress,
		CliqueID:  m.cliqueID,
	}

	return n, nil
}

func (m *ComputeDomainManager) GetNodeIPs(ctx context.Context, cdUID string) ([]string, error) {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return nil, fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return nil, fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	if cd.Status.Nodes == nil {
		return nil, fmt.Errorf("no nodes set for ComputeDomain")
	}

	if len(cd.Status.Nodes) != cd.Spec.NumNodes {
		return nil, fmt.Errorf("not all nodes populated in ComputeDomain status yet")
	}

	var ips []string
	for _, node := range cd.Status.Nodes {
		if m.cliqueID == node.CliqueID {
			ips = append(ips, node.IPAddress)
		}
	}
	return ips, nil
}

func (m *ComputeDomainManager) AssertComputeDomainNamespace(ctx context.Context, claimNamespace, cdUID string) error {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	if cd.Namespace != claimNamespace {
		return fmt.Errorf("the ResourceClaim's namespace is different than the ComputeDomain's namespace")
	}

	return nil
}

func (m *ComputeDomainManager) AddNodeLabel(ctx context.Context, cdUID string) error {
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, m.config.flags.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving Node: %w", err)
	}

	currentValue, exists := node.Labels[computeDomainLabelKey]
	if exists && currentValue != cdUID {
		return fmt.Errorf("label already exists for a different ComputeDomain")
	}

	if exists && currentValue == cdUID {
		return nil
	}

	newNode := node.DeepCopy()
	if newNode.Labels == nil {
		newNode.Labels = make(map[string]string)
	}
	newNode.Labels[computeDomainLabelKey] = cdUID

	if _, err = m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating Node with label: %w", err)
	}
	klog.V(6).Infof("Added node label on %s for CD %s", m.config.flags.nodeName, cdUID)

	return nil
}

func (m *ComputeDomainManager) RemoveNodeLabel(ctx context.Context, cdUID string) error {
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, m.config.flags.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving Node: %w", err)
	}

	if _, exists := node.Labels[computeDomainLabelKey]; !exists {
		return nil
	}

	newNode := node.DeepCopy()
	delete(newNode.Labels, computeDomainLabelKey)

	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating Node to remove label: %w", err)
	}

	return nil
}

func (m *ComputeDomainManager) GetComputeDomain(ctx context.Context, cdUID string) (*nvapi.ComputeDomain, error) {
	cds, err := m.informer.GetIndexer().ByIndex("computeDomainUID", cdUID)
	if err != nil {
		return nil, fmt.Errorf("error retrieving ComputeDomain by UID: %w", err)
	}
	if len(cds) == 0 {
		return nil, nil
	}
	if len(cds) != 1 {
		return nil, fmt.Errorf("multiple ComputeDomains with the same UID")
	}
	cd, ok := cds[0].(*nvapi.ComputeDomain)
	if !ok {
		return nil, fmt.Errorf("failed to cast to ComputeDomain")
	}
	return cd, nil
}

func (m *ComputeDomainManager) periodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			klog.V(6).Infof("Running periodic sync to remove artifacts owned by stale ComputeDomain")

			_, err := os.Stat(m.configFilesRoot)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				klog.Errorf("error checking for existence of directory '%s': %v", m.configFilesRoot, err)
				continue
			}

			entries, err := os.ReadDir(m.configFilesRoot)
			if err != nil {
				klog.Errorf("error reading entries under directory '%s': %v", m.configFilesRoot, err)
				continue
			}

			for _, e := range entries {
				if !e.IsDir() {
					continue
				}

				uid := e.Name()
				path := filepath.Join(m.configFilesRoot, e.Name())

				computeDomain, err := m.GetComputeDomain(ctx, uid)
				if err != nil {
					klog.Errorf("error getting ComputeDomain: %v", err)
					continue
				}

				if computeDomain != nil {
					continue
				}

				klog.Infof("Stale artifacts found for ComputeDomain '%s', running cleanup", uid)

				if err := os.RemoveAll(path); err != nil {
					klog.Errorf("error removing artifacts directory for ComputeDomain '%s': %v", uid, err)
					continue
				}

				if err := m.RemoveNodeLabel(ctx, uid); err != nil {
					klog.Errorf("error removing Node label for ComputeDomain '%s': %v", uid, err)
					continue
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
