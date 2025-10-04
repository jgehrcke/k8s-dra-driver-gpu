#!/bin/bash

set -o nounset

# Wait up to TIMEOUT seconds for the MPI launcher pod to complete successfully.
TIMEOUT=300
SPECPATH="${1:-demo/specs/imex/nvbandwidth-test-job-2.yaml}"
JOB_NAME="${JOB_NAME:-nvbandwidth-test-2-launcher}"

# External supervisor can inject run ID (for many-repetition-tests), used mainly
# in output file names.
RUNID="${RUNID:-no_runid}"

echo "$RUNID -- $SPECPATH"

# Pick one of two fault types.
if (( RANDOM % 2 )); then
    FAULT_TYPE=1
else
    FAULT_TYPE=1
fi

SECONDS=0
FAULT_INJECTED=0
IMEX_DAEMON_LOG_EXTRACTED=0
NVB_COMMS_STARTED=0
LAST_LAUNCHER_RESTART_OUTPUT=""
STATUS="nil"

# Common arguments for `kubectl logs`, with common ts for proper chronological
# sort upon dedup/post-processing.
KLOGS_ARGS="--tail=-1 --prefix --all-containers --timestamps"

LAUNCHER_LOG_PATH="_launcher_logs_${RUNID}.log"
LAUNCHER_ERRORS_LOG_PATH="_launcher_errors_${RUNID}.log"
CDDAEMON_LOG_PATH="_cd-daemon_logs_${RUNID}.log"
#WORKER_LOG_PATH="_worker_logs_dup_${RUNID}.log"


echo "" > "${LAUNCHER_LOG_PATH}"
echo "" > "${LAUNCHER_LOG_PATH}".dup
echo "" > "${CDDAEMON_LOG_PATH}"
echo "" > "${CDDAEMON_LOG_PATH}".dup
#echo "" > "${WORKER_LOG_PATH}"
#echo "" > "${WORKER_LOG_PATH}".dup


_T0=$(awk '{print $1}' /proc/uptime)


log_ts_no_newline() {
    echo -n "$(date -u +'%Y-%m-%dT%H:%M:%S.%3NZ ')"
}

log() {
  _TNOW=$(awk '{print $1}' /proc/uptime)
  _DUR=$(echo "$_TNOW - $_T0" | bc)
  log_ts_no_newline
  printf "[%6.1fs] $1\n" "$_DUR"
}

set -e
log "do: delete -f ${SPECPATH} (and wait)"
kubectl delete -f "${SPECPATH}" --ignore-not-found > /dev/null
kubectl wait --for=delete job/"${JOB_NAME}" --timeout=20s > /dev/null
log "done"

log "do: apply -f ${SPECPATH}"
kubectl apply -f "${SPECPATH}" > /dev/null
log "done"
log "do: wait --for=create"
kubectl wait --for=create job/"${JOB_NAME}" --timeout=40s > /dev/null
log "done"
set +e

kubectl get resourceclaim

CDUID=$(kubectl describe computedomains.resource.nvidia.com nvbandwidth-test-compute-domain-2 | grep UID | awk '{print $2}')
log "CD uid: ${CDUID}"

CD_LABEL_KV="resource.nvidia.com/computeDomain=${CDUID}"

while true; do

    #log "nodes marked with ${CD_LABEL_KV}:"
    #kubectl get nodes -l "$CD_LABEL_KV"
    #kubectl get resourceclaims -A
    #kubectl get pods -l job-name="${JOB_NAME}" -o yaml | grep -e ClaimName -e nodeName
    #kubectl get ds -n nvidia-dra-driver-gpu
    #kubectl get pods -A -o wide | grep dra

    # Log time w/o trailing newline.
    #date -u +"%Y-%m-%dT%H:%M:%S.%3NZ " | sed -z '$ s/\n$//'
    #log "fun"
    # kubectl get pods -o wide
    # echo "logs:"
    # kubectl logs -l job-name=${JOB_NAME} --timestamps --tail=-1 2>&1 | \
    #     grep -e multinode_device_to_device_memcpy_read_ce -e ContainerCreating | \
    #      sed -z '$ s/\n$//'

    # Get restart count (no leading+trailing whitespace, no trailing newline).
    _llro=$( \
        kubectl get pod -l job-name="${JOB_NAME}" -o yaml | \
        grep restartCount | awk '{print $2;}' | tr -d "[:blank:]" | sed 's/\n$//'
    )

    if [[ "$LAST_LAUNCHER_RESTART_OUTPUT" != "$_llro" ]]; then
        log "launcher container restarts seen: $_llro"
        LAST_LAUNCHER_RESTART_OUTPUT="$_llro"
    fi

    # Note that the launcher container may restart various times in the context
    # of this failover. `kubectl logs --follow` does not automatically follow
    # container restarts. To catch all container instances in view of quick
    # restarts, we need to often call a pair of `kubectl logs` commands (once
    # with, and once without --previous). Even that does not reliably obtain
    # _all_ container logs. The correct solution for this type of problem is to
    # have a proper log streaming pipeline. Collect heavily duplicated logs
    # (dedup later)
    kubectl logs -l job-name="${JOB_NAME}" $KLOGS_ARGS $ >> "${LAUNCHER_LOG_PATH}".dup 2>&1
    kubectl logs -l job-name="${JOB_NAME}" $KLOGS_ARGS --previous >> "${LAUNCHER_LOG_PATH}".dup 2>&1

    # Same strategy for CD daemons.
    kubectl logs -n nvidia-dra-driver-gpu -l "$CD_LABEL_KV" $KLOGS_ARGS >> "${CDDAEMON_LOG_PATH}".dup 2>&1
    kubectl logs -n nvidia-dra-driver-gpu -l "$CD_LABEL_KV" $KLOGS_ARGS --previous >> "${CDDAEMON_LOG_PATH}".dup 2>&1

    # Inspect IMEX daemon log (before pods disappear -- happens quickly upon
    # workload completion).
    if (( IMEX_DAEMON_LOG_EXTRACTED == 0 )); then
        # Dump interesting sections of all CD/IMEX daemon logs right after
        # detecting workload success. Detect workload success by searching for a
        # log needle. We just fetched logs above, use that disk state instead of
        # calling `kubectl logs -l job-name=${JOB_NAME} --tail=-1`. If the
        # injected fault involved losing (an) IMEX daemon pod(s) then its/their
        # logs are not collected here.
        if cat "${LAUNCHER_LOG_PATH}".dup 2>&1 | sort | uniq | grep "SUM multinode_device_to_device"; then
            # Fetch logs of all CD/IMEX daemons. Save in files. Filter & show
            # interesting detail inline.
            kubectl get pods -n nvidia-dra-driver-gpu | grep nvbandwidth-test-compute-domain-2 | awk '{print $1}' | while read pname; do
                _logfname="_cd-daemon_${RUNID}_${pname}.log"
                log "CD daemon pod: $pname -- save log to ${_logfname}"

                kubectl logs -n nvidia-dra-driver-gpu "$pname" \
                    --timestamps --prefix --all-containers \
                    > "${_logfname}"
                cat "${_logfname}" | grep \
                        -e "IP set changed" \
                        -e "Connection established" \
                        -e "updated node" \
                        -e "SIGUSR1" \
                        -e "\[ERROR\]" \
                        -e CUDA
            done
            IMEX_DAEMON_LOG_EXTRACTED=1
        fi
    fi

    STATUS=$(kubectl get pod -l job-name="${JOB_NAME}" -o jsonpath="{.items[0].status.phase}" 2>/dev/null)
    if [ "$STATUS" == "Succeeded" ]; then
        log "nvb completed"
        break
    fi

    # The launcher pod handles many failures internally by restarting the
    # launcher container (the MPI launcher process). Treat it as permanent
    # failure when this pod failed overall.
    if [ "$STATUS" == "Failed" ]; then
        log "nvb launcher pod failed"
        break
    fi

    # Keep rather precise track of when the actual communication part of the
    # benchmark has started, T_start. Assume that the benchmark takes at
    # least 20 seconds overall. Inject fault shortly after benchmark has
    # started. Pick that delay to be random (but below 20 seconds).
    if (( NVB_COMMS_STARTED == 1 )); then
        if (( FAULT_INJECTED == 0 )); then
            log "NVB_COMMS_STARTED"

            _jitter_seconds=$(awk -v min=1 -v max=5 'BEGIN {srand(); print min+rand()*(max-min)}')
            log "sleep, pre-injection jitter: $_jitter_seconds s"
            sleep "$_jitter_seconds"

            # Prepare background-running worker log follower to see the failover
            # from the worker's perspective -- this is not particularly chatty.
            # (
            # kubectl logs -l training.kubeflow.org/job-name=nvbandwidth-test-2 \
            #     --tail=-1 --prefix --all-containers --timestamps --follow 2>&1 | grep "/mpi-worker"
            # ) &

            # Note: force-deleting a worker pod does not result reliably in a
            # successful failover. A failover requires a worker to fail _and_
            # for the launcher to quickly notice that (directly, or indirectly).
            # The direct way for the launcher to notice is for the failing
            # worker or for another worker to communicate that failure to the
            # launcher. Regular worker pod deletion results in an MPI worker to
            # see signal 15 which triggers clean shutdown (including clean TCP
            # connection shutdown in the MPI coordination layer). This clean TCP
            # connection shutdown is noticed directly by the launcher, and is
            # treated as an error resulting in a launcher restart. That allows
            # for the launcher (after restart) to pick up a new worker, and
            # after all re-initialize the workload across two workers (of which
            # one is new).
            #
            # Immediate loss of a worker (triggered by SIGKILL) results in
            # unclean TCP connection failure. Here, the other end(s) (launcher,
            # other worker) then probably hang(s) in a recv() system call that
            # is not timeout-controlled. This is a fault scenario that could be
            # caught by the launcher (or the other worker) by implementing a
            # heartbeat and/or after all a reasonable timeout criterion when
            # waiting for the next result to come in. Note that a recv() system
            # call would probably fail after a very long time when subject to
            # the system's TCP stack default timeouts (often very large or
            # quasi-infinite).
            #
            # That is, a SIGKILLed worker simply goes unnoticed and results in a
            # a TIMEOUT for _us_ waiting for some kind of failover to happen.
            #
            # This might actually depend on the exact moment in time for the
            # SIGKILL to arrive. I expect that the SIGKILL (when incoming at the
            # right time) can trigger a CUDA API error in the _other_ worker
            # (not affected by the SIGKILL). In that case, this error would be
            # propagated to the launcher and would again allow for failover.
            # However, maybe the CUDA API-based memory sharing is also affected
            # by SIGKILL and missing timeout control in the same way TCP
            # interaction is: maybe the other worker hangs in a CUDA API call
            # for a long time (that would not seem sane, and the GPU driver/IMEX
            # daemon could prevent this from happening -- by seeing that a
            # process went away).

            # A failing CUDA mem import/export API call happning in a worker process
            # _can_ crash the launcher pod, as is often seen when puand The launcher pod restarts the
            # container. After that, the MPI workload (the benchmark) is
            # reinitialized started again from scratch (I assume that while the
            # MPI worker processes stay alive, they actually start new workload
            # child processes). This type of launcher restart (as of failing TCP
            # interaction with the missing worker) is what after all facilitates
            # healing the workload -- but it does not continue from previously
            # checkpointed state, it starts from scratch.

            if (( FAULT_TYPE == 1 )); then
                log "inject fault type 1: delete worker pod"
                kubectl delete pod nvbandwidth-test-2-worker-0 --grace-period=0 --force
            else
                log "inject fault type 2: force-delete imex daemon"
                kubectl delete pod -n nvidia-dra-driver-gpu \
                    -l resource.nvidia.com/computeDomain --grace-period=0 --force
            fi

            log "'delete pod' cmd returned"
            FAULT_INJECTED=1
            # kubectl wait --for=delete pods nvbandwidth-test-2-worker-0 &
        fi
        # Fault already injected
    else
        # Consult _current_ pod/container.
        if kubectl logs -l job-name="${JOB_NAME}" --tail=-1 2>&1 | grep "Running multinode_"; then
            NVB_COMMS_STARTED=1
        fi
    fi

    if [ "$SECONDS" -ge $TIMEOUT ]; then
        log "global deadline reached ($TIMEOUT seconds), leave control loop"
        break
    fi

    sleep 1
done


log "wait for child processes"
wait

log "dedup launcher logs"
cat "${LAUNCHER_LOG_PATH}".dup | sort | uniq > "${LAUNCHER_LOG_PATH}"

log "dedup CD daemon logs"
cat "${CDDAEMON_LOG_PATH}".dup | sort | uniq > "${CDDAEMON_LOG_PATH}"

log "errors in / reported by launcher:"
cat "${LAUNCHER_LOG_PATH}" | \
    grep -e CUDA_ -e "closed by remote host" -e "Could not resolve" > "${LAUNCHER_ERRORS_LOG_PATH}"
cat "${LAUNCHER_ERRORS_LOG_PATH}"

kubectl logs -n nvidia-dra-driver-gpu \
    -l nvidia-dra-driver-gpu-component=controller \
    --tail=4000 --prefix --all-containers --timestamps | \
    grep 'Removed label'

if [ "$STATUS" != "Succeeded" ]; then
    log "last launcher pod status is not "Succeeded": $STATUS"
    log "finished: failure"
    log "exit with code 1"
    exit 1
fi

log "finished: success"
