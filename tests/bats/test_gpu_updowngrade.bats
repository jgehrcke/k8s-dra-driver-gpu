# shellcheck disable=SC2148
# shellcheck disable=SC2329

setup_file() {
  load 'helpers.sh'
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}


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
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}


# bats test_tags=fastfeedback
@test "GPUs: upgrade: wipe-state, install-last-stable, upgrade-to-current-dev" {
  # Stage 1: clean slate
  helm uninstall "${TEST_HELM_RELEASE_NAME}" -n nvidia-dra-driver-gpu --wait --timeout=30s
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=nvidia-dra-driver-gpu --timeout=10s
  bash tests/bats/clean-state-dirs-all-nodes.sh
  kubectl get crd computedomains.resource.nvidia.com
  timeout -v 10 kubectl delete crd computedomains.resource.nvidia.com

  # Stage 2: install last-stable (this guarantees to install the "old" CRD)
  iupgrade_wait "${TEST_CHART_LASTSTABLE_REPO}" "${TEST_CHART_LASTSTABLE_VERSION}" NOARGS

  # Stage 3: apply workload, do not delete (users care about old workloads to
  # ideally still run, but at the very least be deletable after upgrade).
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=8s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"
  echo "${output}" | wc -l | grep 1

  # Stage 4: update CRD, as a user would do.
  kubectl apply -f "${CRD_UPGRADE_URL}"

  # Stage 5: install target version (as users would do).
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Stage 6: confirm deleting old workload works (critical, see above).
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s

  # Stage 7: fresh create-confirm-delete workload cycle.
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=8s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s
}
