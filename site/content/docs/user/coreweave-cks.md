---
title: "CoreWeave CKS with InfiniBand"
date: 2026-08-26T00:00:00Z
---

DRANET can publish CoreWeave Kubernetes Service (CKS) InfiniBand HCAs through
Kubernetes Dynamic Resource Allocation (DRA). The native CKS provider reads
the local Kubernetes Node labels instead of an instance metadata service and
adds fabric, superpod, leafgroup, rack, speed, and per-HCA leaf-switch
attributes to the `dra.net` ResourceSlice.

The complete two-node B200 NCCL test is under
[`examples/coreweave_cks_examples/b200-infiniband`](https://github.com/kubernetes-sigs/dranet/tree/main/examples/coreweave_cks_examples/b200-infiniband).

## Safe InfiniBand discovery

Run DRANET with `--move-ib-interfaces=false` for the initial CKS InfiniBand
workflow. DRANET then publishes each HCA as an IB-only RDMA device and leaves
the host IPoIB network interface in place. When a pod receives a DRA claim,
DRANET injects only the allocated HCA's `/dev/infiniband` character devices
through NRI; the workload does not need to be privileged.

The example `DeviceClass` selects only CKS backend fabric HCAs:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: infiniband.cks.coreweave.com
spec:
  selectors:
  - cel:
      expression: >-
        device.driver == "dra.net" &&
        device.attributes["dra.net"].rdma == true
  - cel:
      expression: >-
        "coreweave.dra.net" in device.attributes &&
        device.attributes["coreweave.dra.net"].fabricFlavor == "infiniband" &&
        "leafSwitch" in device.attributes["coreweave.dra.net"]
```

The `leafSwitch` presence check is important on B200 nodes because they may
also contain RDMA-capable internal devices which are not connected to the
backend InfiniBand fabric.

## Validated workflow

The example was validated on Kubernetes 1.36 and two 8-GPU B200 CKS nodes.
Each worker received one GPU and one `ibp0` claim that reached
`RDMAOnlyDeviceReady`. CoreWeave's NCCL test image exercised the cross-node
InfiniBand data path through `NET/IBext_v11/0/GDRDMA`, completed with zero data
errors, averaged 41.72 GB/s bus bandwidth, and reached 46.35 GB/s at a 1 GiB
message size.

See the example README for the two-node Helm installation, topology-aware CEL
selectors, NCCL commands, observed results, and cleanup procedure.
