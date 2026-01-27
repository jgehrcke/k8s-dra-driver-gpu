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

import "time"

const (
	// Detecting when a CD daemon transitions from NotReady to Ready (based on
	// the startup probe) at the moment sometimes requires an informer resync,
	// see https://github.com/NVIDIA/k8s-dra-driver-gpu/issues/742.
	informerResyncPeriod = 4 * time.Minute
	mutationCacheTTL     = time.Hour

	// Label keys for ComputeDomainClique objects.
	computeDomainLabelKey       = "resource.nvidia.com/computeDomain"
	computeDomainCliqueLabelKey = "resource.nvidia.com/computeDomain.cliqueID"
)

// IPSet is a set of IP addresses.
type IPSet map[string]struct{}
