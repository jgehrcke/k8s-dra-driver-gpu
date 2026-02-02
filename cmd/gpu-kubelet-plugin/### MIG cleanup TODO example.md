### MIG cleanup TODO example

Pod hangs in ContainerCreating:

```
Events:
  Type     Reason                         Age                From               Message
  ----     ------                         ----               ----               -------
  Normal   Scheduled                      11m                default-scheduler  Successfully assigned default/pod-memsat-0004-mig-070gb-8g9dwhd to gb-nvl-027-compute09
  Warning  FailedPrepareDynamicResources  11m                kubelet            Failed to prepare dynamic resources: prepare dynamic resources: NodePrepareResources failed for ResourceClaim pod-memsat-0004-mig-070gb-8g9dwhd-gpu-tpghv: error preparing devices for claim default/pod-memsat-0004-mig-070gb-8g9dwhd-gpu-tpghv:3c8337ad-f366-4541-883d-30b9e99fd6be: unable to create CDI spec file for claim: failed to construct MIG device DeviceNode edits: failed to parse GI capabilities file /proc/driver/nvidia/capabilities/gpu0/mig/gi2/access: open /proc/driver/nvidia/capabilities/gpu0/mig/gi2/access: no such file or directory
  Warning  FailedPrepareDynamicResources  86s (x8 over 10m)  kubelet            Failed to prepare dynamic resources: prepare dynamic resources: NodePrepareResources failed for ResourceClaim pod-memsat-0004-mig-070gb-8g9dwhd-gpu-tpghv: error preparing devices for claim default/pod-memsat-0004-mig-070gb-8g9dwhd-gpu-tpghv:3c8337ad-f366-4541-883d-30b9e99fd6be: prepare devices failed: error creating MIG device: error creating GPU instance for 'gpu-2-mig-4g95gb-5-0': Insufficient Resources
```


So:
```
error creating GPU instance for 'gpu-2-mig-4g95gb-5-0': Insufficient Resources
```

Relevant checkpoint state on that node (compute09):

```
# cat checkpoint.json |/usr/bin/jq
{
  "checksum": 1083090442,
  "v1": {},
  "v2": {
    "checksum": 469893951,
    "preparedClaims": {
      "06c06dcf-8baf-4024-929b-1aeca5630219": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-18gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-1-mig-1g24gb-19-0"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0010-mig-018gb-4cjnh0r",
              "uid": "3122105a-8852-4821-b57c-ceb6addc619f"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0010-mig-018gb-4cjnh0r-gpu-tztb2",
        "namespace": "default"
      },
      "2af716c2-9c4b-445f-938f-a585e391b74b": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-42gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-2-mig-2g47gb-14-0"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0007-mig-042gb-krbs02g",
              "uid": "bb32932e-0fae-4304-b621-ae1c39e34683"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0007-mig-042gb-krbs02g-gpu-xxcxv",
        "namespace": "default"
      },
      "38aff6f7-11a5-4dbf-a9fb-226ebec453ef": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-45gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-1-mig-2g47gb-14-2"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0001-mig-045gb-grngvdm",
              "uid": "5dc45d2e-2a04-4f81-9e65-d9761cab7226"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0001-mig-045gb-grngvdm-gpu-z6h5l",
        "namespace": "default"
      },
      "3c8337ad-f366-4541-883d-30b9e99fd6be": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-70gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-2-mig-4g95gb-5-0"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0004-mig-070gb-8g9dwhd",
              "uid": "c5501165-442a-4d7d-956f-ac2eb1ff1294"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0004-mig-070gb-8g9dwhd-gpu-tpghv",
        "namespace": "default"
      },
      "67cd7bf4-7bba-48cd-bb8e-9ece016a7f6a": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-70gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-3-mig-4g95gb-5-0"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0002-mig-070gb-53d7m35",
              "uid": "15a77a1f-210d-4327-8bd7-96cdcf018072"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0002-mig-070gb-53d7m35-gpu-vvwb9",
        "namespace": "default"
      },
      "6f04a69a-96a1-4662-9d32-d0881d43397e": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-41gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-1-mig-2g47gb-14-0"
                }
              ]
            },
            "nodeSelector": {
              "nodeSelectorTerms": [
                {
                  "matchFields": [
                    {
                      "key": "metadata.name",
                      "operator": "In",
                      "values": [
                        "gb-nvl-027-compute09"
                      ]
                    }
                  ]
                }
              ]
            }
          },
          "reservedFor": [
            {
              "resource": "pods",
              "name": "pod-memsat-0008-mig-041gb-f9f3b4x",
              "uid": "49280263-b890-4a68-92e1-4027dce217d8"
            }
          ]
        },
        "preparedDevices": null,
        "name": "pod-memsat-0008-mig-041gb-f9f3b4x-gpu-z8rlh",
        "namespace": "default"
      }
    }
  }
}
```

So, that claim is in prepareStarted:
```
      "3c8337ad-f366-4541-883d-30b9e99fd6be": {
        "checkpointState": "PrepareStarted",
        "status": {
          "allocation": {
            "devices": {
              "results": [
                {
                  "request": "req-mig-min-70gb",
                  "driver": "gpu.nvidia.com",
                  "pool": "gb-nvl-027-compute09",
                  "device": "gpu-2-mig-4g95gb-5-0"
                }
              ]
            },
```


The relevant resource claim is gone:
```
07:57:40 ± kubectl get resourceclaim | grep '041gb-f9f3b4x' | wc -l
0
```

On the node, in the gpu plugin log we see:

```
I0201 15:35:30.446443       1 cleanup.go:186] Checkpointed RC cleanup: partially prepared claim not stale: default/pod-memsat-0008-mig-041gb-f9f3b4x-gpu-z8rlh:6f04a69a-96a1-4662-9d32-d0881d43397e
...
I0201 15:45:30.452168       1 cleanup.go:159] Checkpointed RC cleanup: partially prepared claim 'default/pod-memsat-0008-mig-041gb-f9f3b4x-gpu-z8rlh:6f04a69a-96a1-4662-9d32-d0881d43397e' is stale: not found in API server
```

However, we don't pull through with that right:

```
I0201 15:45:30.452221       1 device_state.go:325] Unprepare() for claim 'default/pod-memsat-0008-mig-041gb-f9f3b4x-gpu-z8rlh:6f04a69a-96a1-4662-9d32-d0881d43397e'
I0201 15:45:30.452557       1 device_state.go:369] unprepare noop: claim preparation started but not completed for claim 'default/pod-memsat-0008-mig-041gb-f9f3b4x-gpu-z8rlh:6f04a69a-96a1-4662-9d32-d0881d43397e' (devices: [{req-mig-min-41gb gpu.nvidia.com gb-nvl-027-compute09 gpu-1-mig-2g47gb-14-0 <nil> [] [] [] <nil> map[]}])
```

And so the stale/dangling MIG device keeps floating around in perpetuity.


------

## Cleanup methodology

There are now three cleanup routines / phases:

- Upon plugin startup, before accepting requests
    - DestroyUnknownMIGDevices()