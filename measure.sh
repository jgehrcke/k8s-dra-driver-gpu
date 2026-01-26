#!/bin/bash

source tests/bats/helpers.sh

export N="$1"
yq -i -y "select(.kind == \"ComputeDomain\").spec.numNodes = $N" demo/specs/imex/channel-injection.yaml
cat demo/specs/imex/channel-injection.yaml | grep numNodes

make image-build-and-copy-to-nodes

helm uninstall -n nvidia-dra-driver-gpu nvidia-dra-driver-gpu --wait
helm install nvidia-dra-driver-gpu deployments/helm/nvidia-dra-driver-gpu/ \
    --create-namespace \
    --namespace nvidia-dra-driver-gpu \
    --set resources.gpus.enabled=false \
    --set nvidiaDriverRoot=/run/nvidia/driver \
    --set featureGates.IMEXDaemonsWithDNSNames=true \
    --set logVerbosity=6 \
    --wait


kubectl delete -f demo/specs/imex/channel-injection.yaml --ignore-not-found=true
sleep 2

kubectl apply -f demo/specs/imex/channel-injection.yaml
echo "workload spec applied at $(date -u +%Y-%m-%dT%H:%M:%S%z)"

# time0: workload applied
read time_0 _ < /proc/uptime

i=0

# TODO: wait for first pod to be running (matters for large N), may take a while
# and we do not want to count that to the convergence time.
while true; do
    ((i++))
    sleep 0.2
    output=$(kubectl get deployments.apps -A | grep imex-channel-injection)

    if [[ $output != *"nvidia-dra-driver-gpu"* ]]; then
        # this may be too early; the deployment not net shown in the output.
        continue
    fi

    if [[ $output != *"0/$N"* ]]; then
        echo "$output"
        echo "first pod started"
        break
    fi

    if (( i % 5 == 0 )); then
        echo "$output"
    fi
done
# time0: first pod started (well, probably first pod READY)
read time_1 _ < /proc/uptime

while true; do
    ((i++))
    output=$(kubectl get deployments.apps -A | grep imex-channel-injection)

    if [[ $output == *"$N/$N"* ]]; then
        echo "done"
        echo $output
        break
    fi

    if (( i % 5 == 0 )); then
        echo "$output"
    fi
    sleep 0.3
done
read time_end _ < /proc/uptime
echo "matching pod count dropped to zero"

# Now, also wait for the CD object to reflect per-clique and global readiness.
while true; do
    ((i++))
    output=$(kubectl get computedomains.resource.nvidia.com imex-channel-injection -o yaml)

    if [[ $output != *"NotReady"* ]]; then
        echo "CD is Ready"
        break
    fi

    if (( i % 5 == 0 )); then
        echo "NotReady still in CD"
    fi

    sleep 0.2
done
read time_cd_ready _ < /proc/uptime

duration0=$(echo "$time_end - $time_0" | bc)
duration1=$(echo "$time_end - $time_1" | bc)
duration2=$(echo "$time_cd_ready - $time_1" | bc)

echo "Time from apply to all-pods-READY T_aapr: $duration0 seconds"
echo "Time from first-pod-READY to all-pods-READY T_fprapr: $duration1 seconds"
echo "Time from first-pod-READY to CD-READY T_fprapr: $duration2 seconds"

echo -e "\nclique distribution\n#pods | clique ID"
kubectl get computedomains.resource.nvidia.com imex-channel-injection -o json | \
    jq -r '.status.nodes[].cliqueID' | \
    sort | \
    uniq -c

get_all_cd_daemon_logs_for_cd_name imex-channel-injection > _big_n_cd_daemon.logs

cat _big_n_cd_daemon.logs | \
    grep PATCH | \
    grep 'computedomains' | \
    grep '200 OK' | \
    grep -oP 'milliseconds=\K\d+' | \
    uplot hist --nbins 12 --xlabel "count" --ylabel "time (ms)" --title "distribution of 200 OK PATCH latencies (ms)"

cat _big_n_cd_daemon.logs | \
    grep -oP 't_process_start \K[0-9.]+' | \
    uplot hist --nbins 12 --xlabel "count"  --ylabel "time (s)" --title "distribution of t_process_start (s)"

echo "Mean PATCH request latency"
cat _big_n_cd_daemon.logs | \
    grep PATCH | \
    grep 'computedomaincliques' | \
    grep '200 OK' | \
    grep -oP 'milliseconds=\K\d+' | \
    awk '{ sum += $1; count++ } END { printf "%.1f ms\n", sum/count }'

echo "Number of 200 OK'd PATCH requests:"
cat _big_n_cd_daemon.logs | grep PATCH | grep '200 OK' | wc -l

echo "count: 'DNS index collision detected'"
cat _big_n_cd_daemon.logs | grep 'DNS index collision detected' | wc -l
echo "count: 'picked new index'"
cat _big_n_cd_daemon.logs | grep 'picked new index' | wc -l

# Show timings again.
echo "Timings ($N pods):"
echo "Time from apply to all-pods-READY T_aapr: $duration0 seconds"
echo "Time from first-pod-READY to all-pods-READY T_fprapr: $duration1 seconds"
echo "Time from first-pod-READY to CD-READY T_fprcdr: $duration2 seconds"