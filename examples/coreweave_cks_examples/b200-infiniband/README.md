# CKS B200 InfiniBand DRANET example

This example runs NCCL `all_reduce_perf` across two B200 nodes on CoreWeave
Kubernetes Service (CKS). Each worker receives one GPU and one backend
InfiniBand HCA. DRANET publishes the HCA through DRA and injects only its RDMA
character devices into the container through NRI.

The workflow was validated on a CKS cluster running Kubernetes 1.36 with
8-GPU NVIDIA B200 nodes. It intentionally uses
`--move-ib-interfaces=false`: the HCA is exposed as an IB-only RDMA device and
its host IPoIB interface is not moved into the pod network namespace.

## Prerequisites

- A DRANET image containing the native CKS cloud provider.
- Kubernetes DRA using the `resource.k8s.io/v1` API.
- Containerd with NRI enabled and its socket available at `/var/run/nri`.
- CKS B200 nodes carrying `cks.coreweave.com/cluster` and
  `backend.coreweave.cloud/*` topology labels.
- Kubeflow MPI Operator v0.7.0 for the two-node NCCL test:

  ```bash
  kubectl apply --server-side -k \
    "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"
  ```

Confirm the node architecture and topology before installing:

```bash
kubectl get nodes -l node.kubernetes.io/instance-type=b200-8x \
  -L kubernetes.io/arch,backend.coreweave.cloud/fabric,backend.coreweave.cloud/superpod
```

## Install DRANET on two nodes

Choose exactly two idle B200 nodes in the same fabric, superpod, and leafgroup,
then label them for the test:

```bash
kubectl label nodes <node-1> <node-2> dra.net/cks-nccl-test=true

helm upgrade --install dranet-cks-test ../../../deployments/helm/dranet \
  --namespace dranet-e2e \
  --create-namespace \
  --set image.repository=<image-repository> \
  --set image.tag=<image-tag> \
  --set image.pullPolicy=Always \
  --set-string 'nodeSelector.dra\.net/cks-nccl-test=true' \
  --set args.cloudProviderHint=CKS \
  --set args.profileProvider=none \
  --set args.moveIBInterfaces=false \
  --wait
```

Verify the DaemonSet and the resulting `dra.net` ResourceSlice:

```bash
kubectl -n dranet-e2e get daemonset,pods -o wide
kubectl get resourceslices \
  -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,DRIVER:.spec.driver \
  | grep dra.net
```

## CKS topology attributes

The CKS provider publishes these attributes under `coreweave.dra.net`:

| Attribute | Scope | Example from validation |
|---|---|---|
| `fabricFlavor` | Node | `infiniband` |
| `fabric` | Node | `US-EAST-15A-FAB66` |
| `superpod` | Node | `2` |
| `leafgroup` | Node | `856886992745` |
| `leafgroupName` | Node | `90.1-DH1` |
| `rack` | Node | `82` |
| `speedCurrent` | Node | `3200G` |
| `speedExpected` | Node | `3200G` |
| `instanceType` | Node | `b200-8x` |
| `leafSwitch` | HCA | `L90.1.3` |

On the validated node, DRANET published all eight backend HCAs (`ibp0` through
`ibp7`) with leaf-switch values. The `DeviceClass` requires `leafSwitch` to be
present, excluding RDMA-capable internal devices which are not backend fabric
HCAs.

To constrain a claim to a known fabric location, add selectors to the NIC
request in `resource-claim-template.yaml`. For example:

```yaml
selectors:
- cel:
    expression: >-
      device.attributes["coreweave.dra.net"].fabric == "US-EAST-15A-FAB66" &&
      device.attributes["coreweave.dra.net"].superpod == "2" &&
      device.attributes["coreweave.dra.net"].leafgroup == "856886992745"
```

Use values from your own cluster; the identifiers above are validation
examples and are not portable between CKS fabrics.

## Run the two-node NCCL test

The test uses Kubeflow MPI Operator and CoreWeave's NCCL test image. Confirm
that both nodes publish a `dra.net` ResourceSlice, then apply the class, claim
template, and MPIJob:

```bash
kubectl get resourceslices \
  -o custom-columns=NODE:.spec.nodeName,DRIVER:.spec.driver \
  | grep dra.net

kubectl apply -f device-class.yaml
kubectl -n dranet-e2e apply -f resource-claim-template.yaml
kubectl -n dranet-e2e apply -f mpi-job.yaml

kubectl -n dranet-e2e get pods -o wide
kubectl -n dranet-e2e logs -f \
  -l training.kubeflow.org/job-name=cks-nccl-test-dra,training.kubeflow.org/job-role=launcher
```

Each worker requests one `nvidia.com/gpu` and one
`infiniband.cks.coreweave.com` device. Required pod anti-affinity places the
workers on different nodes, while the test node label confines them to the two
nodes whose DRANET instances publish CKS topology. `NCCL_SHM_DISABLE=1` and
the cross-node placement ensure the collective uses the InfiniBand transport
rather than satisfying the test through intra-node NVLink or shared memory.

### Validated NCCL result

The test completed successfully across two B200 nodes in the same fabric,
superpod, and leafgroup. Each claim allocated `pci-0000-1a-00-0` (`ibp0`) and
reached `RDMAOnlyDeviceReady`. NCCL reported
`NET/IBext_v11/0/GDRDMA`, confirming that the collective used the
DRA-allocated InfiniBand HCA with GPUDirect RDMA.

| Message size | Out-of-place bus bandwidth | Validation errors |
|---:|---:|---:|
| 8 MiB | 30.33 GB/s | 0 |
| 128 MiB | 43.82 GB/s | 0 |
| 512 MiB | 45.79 GB/s | 0 |
| 1 GiB | 46.35 GB/s | 0 |

Average bus bandwidth across the tested sizes was 41.72 GB/s. These values
document one validation run and are not a general CKS performance guarantee.

## Clean up

Delete the MPIJob first so kubelet calls `NodeUnprepareResources` and releases
the generated claims:

```bash
kubectl -n dranet-e2e delete mpijob.kubeflow.org cks-nccl-test-dra
kubectl -n dranet-e2e delete resourceclaimtemplate cks-infiniband-1nic
kubectl delete deviceclass infiniband.cks.coreweave.com

kubectl label nodes <node-1> <node-2> dra.net/cks-nccl-test-
helm uninstall dranet-cks-test --namespace dranet-e2e
kubectl delete namespace dranet-e2e
```
