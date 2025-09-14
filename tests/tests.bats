setup() {
  load '/bats-libraries/bats-support/load.bash'
  load '/bats-libraries/bats-assert/load.bash'
  load '/bats-libraries/bats-file/load.bash'
}

# Note: bats swallows output of setup upon success (regardless of cmdline args
# such as `--show-output-of-passing-tests`). Ref:
# https://github.com/bats-core/bats-core/issues/540#issuecomment-1013521656 --
# it however does emit output upon failure.
setup_file() {
  # Create Helm repo cache dir, otherwise
  # `Error: INSTALLATION FAILED: mkdir /.cache: permission denied`
  export HELM_REPOSITORY_CACHE=$(mktemp -d -t XXXXX)
  export HELM_REPOSITORY_CONFIG=${HELM_REPOSITORY_CACHE}/repo.cfg

  # Prepare for allowing to run this test suite against k8s with host-provided
  # GPU driver.
  export NVIDIA_DRIVER_ROOT="/run/nvidia/driver"
}

@test "test VERSION_W_COMMIT" {
  run make print-VERSION_W_COMMIT
  assert_output --regexp '^v[0-9]+\.[0-9]+\.[0-9]+-dev-[0-9a-f]{8}$'
  echo "$output"
}

@test "test VERSION_GHCR_CHART" {
  run make print-VERSION_GHCR_CHART
  assert_output --regexp '^[0-9]+\.[0-9]+\.[0-9]+-dev-[0-9a-f]{8}-chart$'
  echo $output
}

@test "confirm no kubelet plugin pods running" {
  run kubectl get pods -A -l nvidia-dra-driver-gpu-component=kubelet-plugin
  echo $output
  [ "$status" -eq 0 ]
  refute_output --partial 'Running'
}

@test "helm install from GHCR (OCI)" {
  helm install nvidia-dra-driver-gpu-batssuite \
    oci://ghcr.io/nvidia/k8s-dra-driver-gpu \
    --version "$(make print-VERSION_GHCR_CHART)" \
    --namespace nvidia-dra-driver-gpu \
    --create-namespace \
    --set nvidiaDriverRoot=${NVIDIA_DRIVER_ROOT} \
    --set resources.gpus.enabled=false \
    --set featureGates.IMEXDaemonsWithDNSNames=true
}

@test "get crd computedomains.resource.nvidia.com" {
  kubectl get crd computedomains.resource.nvidia.com
}

@test "wait for kubelet plugin pods READY" {
  kubectl wait --for=condition=READY pods -A \
    -l nvidia-dra-driver-gpu-component=kubelet-plugin --timeout=10s
}

@test "wait for controller pod READY" {
  kubectl wait --for=condition=READY pods -A \
    -l nvidia-dra-driver-gpu-component=controller --timeout=10s
}

@test "IMEX channel injection (single)" {
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=60s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"
  kubectl delete -f demo/specs/imex/channel-injection.yaml
}

@test "nickelpie (NCCL send/recv/broadcast, 2 pods, 2 nodes, small payload)" {
  cd $BATS_TEST_TMPDIR
  git clone https://github.com/jgehrcke/jpsnips-nv
  cd jpsnips-nv && git checkout fb46298fc7aa5fc1322b4672e8847da5321baeb7
  cd nickelpie/one-pod-per-node/
  bash teardown-start-evaluate-npie-job.sh --gb-per-benchmark 5 --matrix-scale 2 --n-ranks 2
  run kubectl logs --prefix -l job-name=nickelpie-test --tail=-1
  kubectl wait --for=condition=complete --timeout=60s job/nickelpie-test
  kubectl delete -f npie-job.yaml.rendered
  kubectl wait --for=delete  --timeout=60s job/nickelpie-test
  echo $output | grep -E '^.*broadcast-.*RESULT bandwidth: [0-9]+\.[0-9]+ GB/s.*$'
}

@test "downgrade: 25.8.0-dev -> 25.3.1" {
  helm uninstall nvidia-dra-driver-gpu-batssuite -n nvidia-dra-driver-gpu
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=nvidia-dra-driver-gpu --timeout=10s
  helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
  helm install nvidia-dra-driver-gpu-batssuite nvidia/nvidia-dra-driver-gpu \
    --version="25.3.1" \
    --create-namespace \
    --namespace nvidia-dra-driver-gpu \
    --set resources.gpus.enabled=false \
    --set nvidiaDriverRoot=${NVIDIA_DRIVER_ROOT}
  kubectl wait --for=condition=READY pods -A -l nvidia-dra-driver-gpu-component=kubelet-plugin --timeout=10s
  kubectl wait --for=condition=READY pods -A -l nvidia-dra-driver-gpu-component=controller --timeout=10s
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=60s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"
  kubectl delete -f demo/specs/imex/channel-injection.yaml
}

@test "wipe-state, install-25.3.1, upgrade" {
  # wipe
  helm uninstall nvidia-dra-driver-gpu-batssuite -n nvidia-dra-driver-gpu
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=nvidia-dra-driver-gpu --timeout=10s
  bash tests/clean-state-dirs-all-nodes.sh
  kubectl get crd computedomains.resource.nvidia.com

  # install 25.3.1, confirm working
  helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
  helm install nvidia-dra-driver-gpu-batssuite nvidia/nvidia-dra-driver-gpu \
    --version="25.3.1" \
    --create-namespace \
    --namespace nvidia-dra-driver-gpu \
    --set resources.gpus.enabled=false \
    --set nvidiaDriverRoot=${NVIDIA_DRIVER_ROOT}
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=60s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"
  # kubectl delete -f demo/specs/imex/channel-injection.yaml

  # uninstall regularly (leaves state behind on nodes, etc)
  helm uninstall nvidia-dra-driver-gpu-batssuite -n nvidia-dra-driver-gpu
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=nvidia-dra-driver-gpu --timeout=10s

  # install dev version (upgrade, as users would do it)
  helm install nvidia-dra-driver-gpu-batssuite \
    oci://ghcr.io/nvidia/k8s-dra-driver-gpu \
    --version "$(make print-VERSION_GHCR_CHART)" \
    --namespace nvidia-dra-driver-gpu \
    --create-namespace \
    --set nvidiaDriverRoot=${NVIDIA_DRIVER_ROOT} \
    --set resources.gpus.enabled=false

  # confirm working
  kubectl apply -f demo/specs/imex/channel-injection-all.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection-all --timeout=60s
  run kubectl logs imex-channel-injection-all
  assert_output --partial "channel1337"

  # delete both workloads
  kubectl delete -f demo/specs/imex/channel-injection-all.yaml
  kubectl delete -f demo/specs/imex/channel-injection.yaml
}

@test "IMEX channel injection (all)" {
  kubectl apply -f demo/specs/imex/channel-injection-all.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection-all --timeout=60s
  run kubectl logs imex-channel-injection-all
  assert_output --partial "channel2047"
  assert_output --partial "channel222"
  kubectl delete -f demo/specs/imex/channel-injection-all.yaml
}

@test "nvbandwidth (2 nodes, 2 GPUs each)" {
  cd $BATS_TEST_TMPDIR
  kubectl create -f https://github.com/kubeflow/mpi-operator/releases/download/v0.6.0/mpi-operator.yaml || echo "ignore"
  kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-dra-driver-gpu/refs/heads/main/demo/specs/imex/nvbandwidth-test-job-1.yaml
  # The canonical k8s job interface works even for this MPIJob
  # (the MPIJob has an underlying k8s job).
  kubectl wait --for=create job/nvbandwidth-test-1-launcher --timeout=10s
  kubectl wait --for=condition=complete job/nvbandwidth-test-1-launcher --timeout=60s
  run kubectl logs --tail=-1 --prefix -l job-name=nvbandwidth-test-1-launcher
  kubectl delete -f https://raw.githubusercontent.com/NVIDIA/k8s-dra-driver-gpu/refs/heads/main/demo/specs/imex/nvbandwidth-test-job-1.yaml
  echo $output | grep -E '^.*SUM multinode_device_to_device_memcpy_read_ce [0-9]+\.[0-9]+.*$'
}