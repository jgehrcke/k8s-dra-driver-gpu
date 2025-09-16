#!/bin/bash
#
#  SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
#  SPDX-License-Identifier: Apache-2.0
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.


set -o nounset
set -o pipefail

# For debugging: state of the world
kubectl get computedomains.resource.nvidia.com
kubectl get pods -n nvidia-dra-driver-gpu
helm list -A

# Some effort to delete workloads potentially left-over from a previous
# interrupted run. TODO: try to affect all-at-once, maybe with a special label.
# Note: the following commands are OK to fail -- the `errexit` shell option is
# deliberately not set here.
timeout -v 5 kubectl delete -f demo/specs/imex/channel-injection.yaml
timeout -v 5 kubectl delete -f demo/specs/imex/channel-injection-all.yaml
timeout -v 5 kubectl delete -f demo/specs/imex/channel-injection-no-numnodes.yaml
timeout -v 5 kubectl delete jobs nickelpie-test
timeout -v 5 kubectl delete computedomain nickelpie-test-compute-domain
timeout -v 5 kubectl delete -f https://raw.githubusercontent.com/NVIDIA/k8s-dra-driver-gpu/refs/heads/main/demo/specs/imex/nvbandwidth-test-job-1.yaml

# Delete any previous remainder of `clean-state-dirs-all-nodes.sh` invocation.
kubectl delete pods privpod-rm-plugindirs

helm uninstall nvidia-dra-driver-gpu-batssuite -n nvidia-dra-driver-gpu

set -x
kubectl wait \
    --for=delete pods -A \
    -l app.kubernetes.io/name=nvidia-dra-driver-gpu \
    --timeout=10s \
        || echo "wait-for-delete failed"

# The next `helm install` must freshly install CRDs, and this is one way to try
# to achieve that. This might time out in case workload wasn't cleaned up
# properly.
timeout -v 10 kubectl delete crds computedomains.resource.nvidia.com || echo "CRD deletion failed"

# Remove kubelet plugin state directories from all nodes.
bash tests/clean-state-dirs-all-nodes.sh
set +x
