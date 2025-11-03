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

@test "simple gpu" {
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs \
    -l nvidia-dra-driver-gpu-component=kubelet-plugin \
    -n nvidia-dra-driver-gpu \
    -c gpus \
    --prefix --tail=-1
  assert_output --partial "About to announce device gpu-0"

  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  local _podname="pod-full-gpu"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=10s
  run kubectl logs "${_podname}"

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 1

  kubectl delete -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=delete pods "${_podname}" --timeout=10s
}
