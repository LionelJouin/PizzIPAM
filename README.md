# Controllerless IPAM

A proof-of-concept IP address manager for Kubernetes with **no controller**. The
API server itself allocates addresses at admission time using a
`MutatingAdmissionPolicy` (CEL), with a `ValidatingAdmissionPolicy` as the
correctness backstop.

```yaml
apiVersion: mystuff.com/v1
kind: IPRequest
metadata:
  name: devicenetwork-0
spec:
  network: abc
  subnet: 192.168.0.0/24
  objectSubnet: 192.168.0.0/25   # this object owns one /25 block
  baseInt: 3232235520            # int(192.168.0.0)
  size: 128
  requests: [alpha, beta]
status:
  ips:                           # filled by the API server, no controller
  - {request: alpha, ip: 3232235521, display: 192.168.0.1}
  - {request: beta,  ip: 3232235522, display: 192.168.0.2}
```

## How it works

1. **One object per block.** All allocations for a block live in a single
   `IPRequest`. Concurrent writers therefore collide on that object's
   `resourceVersion`, so etcd's compare-and-swap serializes them (the loser gets
   HTTP 409 and retries). This is what makes allocation *atomic* without a lock.

2. **IPs stored as integers.** Working in integer space (not dotted strings)
   makes the CEL arithmetic trivial. Under the append-only model the used set is
   a contiguous prefix, so the next free address is just `firstFree = lo + count(used)`.
   A dotted-decimal `display` field is derived with plain integer division/modulo.

3. **Sharding for scale.** A single object is capped by etcd's ~1.5 MiB object
   size and by write contention, so address space is split into fixed, disjoint
   blocks (`/25`s here). Disjoint blocks mean per-IP uniqueness stays *local* to
   each object — no cross-object checks needed.

4. **Validation is local and sound.** The `ValidatingAdmissionPolicy` runs after
   the mutation and checks only this object: all requests allocated, IPs in
   range, no duplicates.

## Files

| File | Purpose |
|------|---------|
| `kind-config.yaml` | kind cluster with the alpha feature enabled |
| `manifests/crd.yaml` | the `IPRequest` CRD (status subresource intentionally OFF) |
| `manifests/mutating-policy.yaml` | the allocator (CEL) |
| `manifests/validating-policy.yaml` | the correctness backstop |
| `examples/shard-0.yaml`, `shard-1.yaml` | two `/25` shards of `192.168.0.0/24` |
| `demo.sh` | one-shot end-to-end run |

## Run

```bash
./demo.sh                         # creates a kind cluster and walks through it
# or manually:
kind create cluster --config kind-config.yaml
kubectl apply -f manifests/crd.yaml
kubectl apply -f manifests/validating-policy.yaml
kubectl apply -f manifests/mutating-policy.yaml
kubectl apply -f examples/shard-0.yaml
kubectl get iprequest -o yaml
```

## Version note

`MutatingAdmissionPolicy` is **alpha** (introduced in k8s 1.32; `v1alpha1`) and
must be enabled with a feature gate + `runtime-config` — done in
`kind-config.yaml`. It is **not available on managed clusters** (GKE/EKS/AKS) out
of the box. If your cluster ships it as **beta**, switch the API version to
`v1beta1` and drop the feature gate (see the note in `kind-config.yaml`).
`ValidatingAdmissionPolicy` is GA and needs nothing.

## Deliberate limitations (this is a demo, not production IPAM)

- **IPv4 only.** IPv6 is 128-bit and overflows CEL's 64-bit integers; the clean
  integer trick doesn't apply.
- **No release/reclaim.** Apply-merge is additive, so removing a request from
  `spec.requests` does *not* free its IP. To reclaim, delete the object.
- **`status` is not RBAC-protected.** With the status subresource off, a client
  could hand-write `status`; the ValidatingAdmissionPolicy only checks
  consistency, it can't reserve status writes to the policy.
- **Routing + 409 retries are client-side.** Choosing a shard, spilling over
  when one is full, and retrying on conflict live in whatever applies the object
  (a `kubectl` wrapper) — "no controller", not "no client".
- **Batch cap of 32 new requests per apply** (a CEL literal, not a block limit);
  add more across multiple applies.
- **Block size** is bounded by etcd object size — comfortable to a `/20`-ish;
  shard further beyond that.
