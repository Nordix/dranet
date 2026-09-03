# CoreWeave Kubernetes Service + DRANET examples

End-to-end examples of topology-aware RDMA device allocation on CoreWeave
Kubernetes Service (CKS) using Kubernetes Dynamic Resource Allocation (DRA).

| Example | Node type | Fabric | Validation |
|---|---|---|---|
| [`b200-infiniband/`](b200-infiniband/) | 8 x NVIDIA B200 | 8 x 400 Gb/s InfiniBand HCA | Two-node NCCL all-reduce over a DRA-allocated HCA |

CKS exposes its network topology through Kubernetes Node labels. The native
CKS provider copies that topology into each network device published by
DRANET, allowing claims to select devices by fabric, superpod, leafgroup, or
leaf switch without querying an instance metadata service.
