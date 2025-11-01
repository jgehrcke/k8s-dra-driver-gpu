# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Executed before entering each test in this file.
setup() {
   load 'helpers.sh'
  _common_setup
  log_objects
}


bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  show_kubelet_plugin_error_logs
  #get_all_cd_daemon_logs_for_cd_name "imex-channel-injection" || true
  echo -e "FAILURE HOOK END\n\n"
}

@test "simple dyn mig" {
  # Confirm that MIG mode is disabled for all GPUs on all nodes.
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
     nvmm $node sh -c 'nvidia-smi --query-gpu=index,mig.mode.current --format=csv'
     run nvmm $node sh -c 'nvidia-smi --query-gpu=index,mig.mode.current --format=csv'
     refute_output --partial "Enabled"
  done

  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs \
    -l nvidia-dra-driver-gpu-component=kubelet-plugin \
    -n nvidia-dra-driver-gpu \
    -c gpus \
    --prefix --tail=-1
  assert_output --partial "About to announce device gpu-0-mig-1g24gb-0"

  kubectl apply -f tests/bats/specs/gpu-simple-mig.yaml
  kubectl wait --for=condition=READY pods pod-mig1g --timeout=10s
  run kubectl logs pod-mig1g

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  #   MIG 1g.24gb     Device  0: (UUID: MIG-5b696ac1-c323-589e-a082-e6045e980bf4)
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 2

  kubectl delete -f tests/bats/specs/gpu-simple-mig.yaml
  kubectl wait --for=delete pods pod-mig1g --timeout=10s

  # Confirm that MIG mode is disabled for all GPUs on all nodes.
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
     run nvmm "$node" sh -c 'nvidia-smi --query-gpu=index,mig.mode.current --format=csv'
     refute_output --partial "Enabled"
  done
}
