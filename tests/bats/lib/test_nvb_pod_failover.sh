#!/bin/bash

MAX_ITER=4

# Wait up to TIMEOUT seconds for the MPI launcher pod to complete successfully.
TIMEOUT=300

SPECPATH="demo/specs/imex/nvbandwidth-test-job-2.yaml"

kubectl delete -f "$SPECPATH"

for ((i=1; i<=MAX_ITER; i++)); do
    echo "Starting iteration $i"
    kubectl apply -f "$SPECPATH"

    # Pick one of two fault types.
    if (( RANDOM % 2 )); then
        FAULT_TYPE=0
    else
        FAULT_TYPE=1
    fi

    SECONDS=0
    FAULT_INJECTED=0
    IMEX_DAEMON_LOG_EXTRACTED=0
    NVB_COMMS_STARTED=0
    LAST_LAUNCHER_RESTART_OUTPUT=""

    _log_follower_pid=0

    echo "" > _launcher_logs_dup.log
    echo "" > _worker_logs_dup.log

    while true; do
        STATUS=$(kubectl get pod -l job-name=nvbandwidth-test-2-launcher -o jsonpath="{.items[0].status.phase}" 2>/dev/null)

        date -u +"%Y-%m-%dT%H:%M:%S.%3NZ " | sed -z '$ s/\n$//'
        kubectl get pods -o wide
        #echo "logs:"
        # kubectl logs -l job-name=nvbandwidth-test-2-launcher --timestamps --tail=-1 2>&1 | \
        #     grep -e multinode_device_to_device_memcpy_read_ce -e ContainerCreating | \
        #      sed -z '$ s/\n$//'

        # Remove leading whitespace and trailing newline.
        _llro=$(kubectl get pod -l job-name=nvbandwidth-test-2-launcher -o yaml | grep restartCount | tr -d "[:blank:]" | sed -z '$ s/\n$//')

        if [[ "$LAST_LAUNCHER_RESTART_OUTPUT" != "$_llro" ]]; then
            echo -n " launcher restarts: $_llro"
            LAST_LAUNCHER_RESTART_OUTPUT="$_llro"
        fi

        echo " "

        # Note that the launcher container may restart various times in the
        # context of this failover. `kubectl logs --follow` does not
        # automatically follow container restarts. To catch all container
        # instances in view of quick restarts, we need to often call a pair of
        # `kubectl logs` commands (once with, and once without --previous). Even
        # that does not reliably obtain _all_ container logs. The correct
        # solution for this type of problem is to have a proper log streaming
        # pipeline.
        # Collect heavily duplicated logs (dedup later)
        kubectl logs -l job-name=nvbandwidth-test-2-launcher --tail=-1 --prefix --all-containers --timestamps >> _launcher_logs_dup.log 2>&1
        kubectl logs -l job-name=nvbandwidth-test-2-launcher --tail=-1 --prefix --all-containers --timestamps --previous >> _launcher_logs_dup.log 2>&1

        # Inspect IMEX daemon log (before pods disappear -- happens quickly upon
        # workload completion).
        if (( IMEX_DAEMON_LOG_EXTRACTED == 0 )); then
            # Dump interesting sections of all IMEX daemon logs right after
            # detecting workload success. Detect workload success by searching
            # for a log needle. We just fetched logs above, use that disk state
            # instead of calling `kubectl logs -l
            # job-name=nvbandwidth-test-2-launcher --tail=-1`
            if cat _launcher_logs_dup.log 2>&1 | sort | uniq | grep "SUM multinode_device_to_device"; then
                # Inspect logs of all IMEX daemons
                kubectl get pods -n nvidia-dra-driver-gpu | grep nvbandwidth-test-compute-domain-2 | awk '{print $1}' | while read pname; do
                    echo "IMEX daemon pod: $pname"
                    kubectl logs -n nvidia-dra-driver-gpu "$pname" --timestamps --prefix --all-containers | \
                        grep -e "IP set changed" -e "Connection established" -e "updated node" -e "SIGUSR1" \
                            -e "\[ERROR\]"
                done
                IMEX_DAEMON_LOG_EXTRACTED=1
            fi
        fi

        # Pod succeeded
        if [ "$STATUS" == "Succeeded" ]; then
            echo "nvb completed"
            break
        fi

        # Pod failed
        if [ "$STATUS" == "Failed" ]; then
            echo "Pod failed."
            break
        fi

        # Keep rather precise track of when the actual communication part of the
        # benchmark has started, T_start. Assume that the benchmark takes at
        # least 20 seconds overall. Inject fault shortly after benchmark has
        # started. Pick that delay to be random (but below 20 seconds).
        if (( NVB_COMMS_STARTED == 1 )); then
            if (( FAULT_INJECTED == 0 )); then
                echo "NVB_COMMS_STARTED_AFTER: $NVB_COMMS_STARTED_AFTER seconds"
                _jitter_seconds=$(awk -v min=1 -v max=15 'BEGIN {srand(); print min+rand()*(max-min)}')
                echo "inject fault (delete pod) after $_jitter_seconds s"
                sleep "$_jitter_seconds"

                # Prepare background-running worker log follower to see the
                # failover from the worker's perspective -- this is not
                # particularly chatty.
                (
                kubectl logs -l training.kubeflow.org/job-name=nvbandwidth-test-2 \
                    --tail=-1 --prefix --all-containers --timestamps --follow 2>&1 | grep "/mpi-worker"
                ) &

                # Note: force-deleting this pod would not result in a successful
                # failover. Failing CUDA mem import/export does _not_ seem to
                # crash the launcher pod, and the system seems to stay faulty
                # forever. Regular worker pod deletion via signal 15 allows for
                # tidy TCP connection shutdown in the MPI coordination layer
                # between launcher and worker. This clean TCP shutdown is
                # noticed by the launcher, resulting in a container crash. The
                # launcher pod restarts the container, and the benchmark is
                # (tried to be) started again from scratch. This type of
                # launcher restart (as of failing TCP interaction with the
                # missing worker) is what after all facilitates healing the
                # workload.

                if (( FAULT_TYPE == 1 )); then
                    echo "inject fault type 1: delete worker pod"
                    kubectl delete pod nvbandwidth-test-2-worker-0
                else
                    echo "inject fault type 2: force-delete imex daemon"
                    kubectl delete pod -n nvidia-dra-driver-gpu \
                        -l resource.nvidia.com/computeDomain --grace-period=0 --force
                fi

                echo "'delete pod' cmd returned"
                FAULT_INJECTED=1
                # kubectl wait --for=delete pods nvbandwidth-test-2-worker-0 &
            fi
            # Fault already injected
        else
            if kubectl logs -l job-name=nvbandwidth-test-2-launcher --tail=-1 2>&1 | grep "Running multinode_"; then
                NVB_COMMS_STARTED=1
                NVB_COMMS_STARTED_AFTER=$SECONDS
                echo "NV_COMMS_STARTED"
            fi
        fi

        # Delete worker pod after $WORKER_DELETE_DELAY seconds
        # elif [ "$SECONDS" -ge $WORKER_DELETE_DELAY ] && [ "$FAULT_INJECTED" = "false" ]; then
        #         kubectl delete pod nvbandwidth-test-2-worker-0
        #         FAULT_INJECTED=true

        # Force delete (i.e. with SIGKILL) IMEX daemon pods after $DAEMON_DELETE_DELAY seconds
        # elif [ "$SECONDS" -ge $DAEMON_DELETE_DELAY ] && [ "$DAEMON_DELETED" = "false" ]; then
        #         kubectl delete pod -n nvidia-dra-driver-gpu -l resource.nvidia.com/computeDomain --grace-period=0 --force
        #         DAEMON_DELETED=true

        # Timeout reached
        if [ "$SECONDS" -ge $TIMEOUT ]; then
            echo "Timeout reached ($TIMEOUT seconds). Exiting loop."
            exit 1
        fi

        sleep 1
    done

    kubectl delete -f $SPECPATH
done

echo "wait for child processes"
wait

echo "dedup launcher logs"
cat _launcher_logs_dup.log | sort | uniq > _launcher_logs_dedup.log

echo "errors in / reported by launcher:"
cat _launcher_logs_dedup.log | \
    grep -e CUDA_ -e "closed by remote host" -e "Could not resolve" > _launcher_errors.log
cat _launcher_errors.log

echo "finished fault injection test ($MAX_ITER iteration(s))"
