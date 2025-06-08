#!/usr/bin/env bash

# Main intent: help users to self-troubleshoot when the GPU driver is not set up
# properly before installing this DRA driver. In that case, the log of the init
# container running this script is meant to yield an actionable error message.
# For now, rely on k8s to implement a high-level retry with back-off.

if [ -z "$NVIDIA_DRIVER_ROOT" ]; then
    # Not set, or set to empty string (not distinguishable).
    # Normalize to "/" (treated as such elsewhere).
    export NVIDIA_DRIVER_ROOT="/"
fi

# Create in-container path /driver-root as a symlink. Pick a different symlink
# target depending on whether the GPU driver is (i) operator-provided or (ii)
# host-provided.
if [ "${NVIDIA_DRIVER_ROOT}" == "/run/nvidia/driver" ]; then
    # Expectation: link may be broken initially if the GPU operator isn't
    # deployed yet. The link heals once GPU operator provides the driver on the
    # host at /run/nvidia/driver. Notably, on the host, the directory
    # /run/nvidia is the mount point that gets created when the GPU operator
    # gets deployed
    echo "create symlink: /driver-root -> /host-run-nvidia/driver"
    ln -s /host-run-nvidia/driver /driver-root
    # stat /driver-root
else
    echo "create symlink: /driver-root -> /host-driver-root"
    ln -s /host-driver-root /driver-root
fi

emit_common_err () {
    printf '%b' \
        "Check failed. Has the NVIDIA GPU driver been set up? " \
        "The GPU driver is expected to be installed under " \
        "NVIDIA_DRIVER_ROOT (currently set to '${NVIDIA_DRIVER_ROOT}') " \
        "in the host filesystem. If that path appears to be unexpected: " \
        "review and adjust the 'nvidiaDriverRoot' Helm chart variable. " \
        "If the value is expected: review if the GPU driver has " \
        "actually been installed under NVIDIA_DRIVER_ROOT.\n"
}

# Goal: relevant log output should repeat over time.
validate_and_exit_on_success () {
    echo -n "$(date -u +"%Y-%m-%dT%H:%M:%SZ")  Search NVIDIA_DRIVER_ROOT ($NVIDIA_DRIVER_ROOT). "

    # Search specific set of directories (don't resursively go through all of
    # /driver-root because that may be a big filesystem). Limit to first result
    # (multiple results are a bit of a pathological state, but instead of
    # erroring out we can try to continue with validation logic). Suppress find
    # stderr: some of those directories are expected to be "not found". Limit to
    # maxdepth 1 to keep search predictable, and to protect against any
    # potential symlink loop (we're suppressing find's stderr, so we'd never see
    # messages like 'Too many levels of symbolic links').

    NV_PATH=$( \
        find \
            /driver-root/bin \
            /driver-root/sbin \
            /driver-root/usr/bin \
            /driver-root/sbin \
        -maxdepth 1 -type f -name "nvidia-smi" 2> /dev/null | head -n1
    )

    # Follow symlinks (-L), because `libnvidia-ml.so.1` is typically a link.
    NV_LIB_PATH=$( \
        find -L \
            /driver-root/usr/lib64 \
            /driver-root/usr/lib/x86_64-linux-gnu \
            /driver-root/usr/lib/aarch64-linux-gnu \
            /driver-root/lib64 \
            /driver-root/lib/x86_64-linux-gnu \
            /driver-root/lib/aarch64-linux-gnu \
        -maxdepth 1 -type f -name "libnvidia-ml.so.1" 2> /dev/null | head -n1
    )

    if [ -z "${NV_PATH}" ]; then
        echo -n "nvidia-smi: not found. "
    else
        echo -n "nvidia-smi: '${NV_PATH}'. "
    fi

    if [ -z "${NV_LIB_PATH}" ]; then
        echo -n "libnvidia-ml.so.1: not found. "
    else
        echo -n "libnvidia-ml.so.1: '${NV_LIB_PATH}'. "
    fi

    # Terminate previous log line.
    echo

    if [ -n "${NV_PATH}" ] && [ -n "${NV_LIB_PATH}" ]; then

        # Run with clean environment (only set LD_PRELOAD, nvidia-smi has only
        # this dependency). Emit message before invocation (nvidia-smi may be
        # slow or hang).
        echo "invoke: env -i LD_PRELOAD=${NV_LIB_PATH} ${NV_PATH}"
        env -i LD_PRELOAD="${NV_LIB_PATH}" "${NV_PATH}"
        RCODE="$?"

        # For checking GPU driver health: rely on nvidia-smi's exit code. Rely
        # on code 0 signaling that the driver is properly set up. See section
        # 'RETURN VALUE' in the nvidia-smi man page for meaning of error codes.
        if [ ${RCODE} -eq 0 ]; then
            echo "nvidia-smi returned with code 0: success, leave"

            # Exit script indicating success (leave init container).
            exit 0
        else
            echo "exit code: ${RCODE}"
        fi
    fi

    # List current set of top-level directories in /driver-root.
    echo "Directory contains: [$(/bin/ls -A1 /driver-root 2>/dev/null | tr '\n' ' ')]."

    # Reduce log volume: log hints only every Nth attempt.
    if [ $((_ATTEMPT % 6)) -ne 0 ]; then
        return
    fi

    # nvidia-smi binaries not found, or execution failed. First, provide generic
    # error message. Then, try to provide actional hints for common problems.
    emit_common_err

    # For host-provided driver not at / provide feedback for two special cases.
    if [ "${NVIDIA_DRIVER_ROOT}" != "/" ]; then
        if [ -z "$( ls -A /driver-root )" ]; then
            echo "Hint: Directory $NVIDIA_DRIVER_ROOT on the host is empty"
        else
            # Not empty, but at least one of the binaries not found: this is a
            # rather pathotlogical state.
            if [ -z "${NV_PATH}" ] || [ -z "${NV_LIB_PATH}" ]; then
                echo "Hint: Directory $NVIDIA_DRIVER_ROOT is not empty but at least one of the binaries wasn't found."
            fi
        fi
    fi

    # Common mistake: driver container, but forgot -set nvidiaDriverRoot
    if [ "${NVIDIA_DRIVER_ROOT}" == "/" ] && [ -f /driver-root/run/nvidia/driver/usr/bin/nvidia-smi ]; then
        printf '%b' \
        "Hint: /run/nvidia/driver/usr/bin/nvidia-smi exists on the host, you " \
        "may want to re-install the DRA driver Helm chart with " \
        "--set nvidiaDriverRoot=/run/nvidia/driver\n"
    fi

    if [ "${NVIDIA_DRIVER_ROOT}" == "/run/nvidia/driver" ]; then
        printf '%b' \
            "Hint: NVIDIA_DRIVER_ROOT is /run/nvidia/driver " \
            "which typically means that the NVIDIA GPU Operator " \
            "manages the GPU driver. Make sure that the Operator " \
            "is deployed and healthy.\n"
    fi
}

shutdown() {
  echo "$(date -u +"%Y-%m-%dT%H:%M:%S.%3NZ"): received SIGTERM"
  exit 0
}

trap 'shutdown' SIGTERM


# Design goal: long-running init container that retries at constant frequency,
# and leaves only upon success (with code 0).
_WAIT_S=10
_ATTEMPT=0

while true
do
    validate_and_exit_on_success
    # echo "retry in ${_WAIT_S} s"
    sleep ${_WAIT_S}
    _ATTEMPT=$((_ATTEMPT+1))
done
