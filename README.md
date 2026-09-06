# PizzIPAM

> **A controllerless, webhook-free, lease-free IP Address Manager for Kubernetes.**  
> The Kubernetes API server itself allocates IP addresses at admission time using in-tree CEL (**`MutatingAdmissionPolicy`**).

```
[ Client / CNI / DRA ]
          │
          │  1. Single Server-Side Apply (No prior GET / LIST)
          ▼
  [ kube-apiserver ]
          │
          ├─► MutatingAdmissionPolicy (CEL) ──► Allocates IP at admission time
          ├─► ValidatingAdmissionPolicy (CEL) ──► Enforces naming & invariants
          ▼
[ Instant IP in apply response ]
```

---

## Why PizzIPAM?

* **⚡ Zero Controllers, Zero Webhook Pods:** No controller pods to manage, crash, or scale.
* **🔒 Zero Distributed Locks:** No `coordination.k8s.io` Leases or etcd locks. Elimination of cluster-wide lease deadlocks.
* **🚀 Single Round-Trip (`apply -o yaml`):** The client applies its request and gets the allocated IP immediately in the response body. No async polling, no watching claims.
* **🎯 Deterministic Naming:** Slices are canonically named from their block identity (`pkg/naming`). Clients address slices directly with **no prior `GET` or `LIST`**.
* **🌐 True Dual-Stack (IPv4 & IPv6):** Operates on a family-agnostic host offset model (`0..63`) supporting both IPv4 (`/26`) and IPv6 (`/122`).
* **🏎️ Extreme Performance:** > 1600 alloc/s on spread pools.

---

## Quick Example

Request an IP using standard Server-Side Apply:

```yaml
kubectl apply --server-side --field-manager=pod-1 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: blue-net.pod.ipv4-10-0-0-0-26
spec:
  podNetworkRef:
    kind: pod
    name: blue-net
  sliceSubnet:
    family: IPv4
    prefix: 10.0.0.0
    prefixLength: 26
  request:
  - name: pod-1
EOF
```

The API server synchronously populates `status.allocation` and `status.bitmap` before writing to etcd:

```yaml
status:
  bitmap: "1000000000000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-1
    offset: 0
    address: 10.0.0.0
```

---

## Quickstart

### 1. Requirements
* Kubernetes **v1.32+** with `MutatingAdmissionPolicy` enabled:
  * Runtime config: `admissionregistration.k8s.io/v1alpha1=true` (or beta/GA depending on k8s version).
  * Feature gate: `MutatingAdmissionPolicy=true`.

### 2. Install
```bash
kubectl apply -f deployment/
```
This installs:
1. `IPSlice` CustomResourceDefinition.
2. `MutatingAdmissionPolicy` (the in-tree CEL allocator).
3. `ValidatingAdmissionPolicy` (the correctness backstop).

### 3. Go Client Helper
For programmatic integration (e.g. CNI plugins or DRA drivers), PizzIPAM provides a client helper that automates deterministic naming, multi-slice walking, and collision hopping:

```go
import "github.com/lioneljouin/pizzipam/pkg/allocator"

addr, err := allocator.Allocate(ctx, client, networkRef, subnet, "my-pod", allocator.WithOrder(allocator.Random))
// -> returns netip.Addr (e.g. 10.0.0.0) in one self-describing apply round-trip
```

---

## Deep Dives

* **[The 9 Load-Bearing Rules](docs/readme.md)**: Architectural guarantees that make allocation correct and collision-free without a controller.
* **[How It Works Under the Hood](docs/how-it-works.md)**: The internal lifecycle, Server-Side Apply mechanics, CEL execution pipeline, and `GuaranteedUpdate` concurrency loops.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.