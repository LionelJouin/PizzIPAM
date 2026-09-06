# How PizzIPAM Works

PizzIPAM is a **controllerless IP address manager (IPAM)** for Kubernetes. Unlike traditional IPAM solutions (which run central controller pods, daemonsets, or cluster-wide distributed lease locks), PizzIPAM delegates address allocation directly to the **Kubernetes API Server** at admission time using **MutatingAdmissionPolicy** (CEL), with a **ValidatingAdmissionPolicy** as the correctness backstop.

---

## The Core Philosophy: Zero Coordination, Single Round-Trip

When a consumer (e.g. a CNI plugin, DRA driver, or pod) needs an IP address:
1. It **does not** query the API server with a `GET` or `LIST` to find an available slice.
2. It **does not** wait for an asynchronous controller to populate a status field.
3. It **does not** acquire a cluster-wide distributed lock (lease).

Instead, the client calculates the target slice's name deterministically from its block identity, issues a single **Server-Side Apply (SSA)** request, and receives the allocated IP directly in the response:

```sh
$ kubectl apply --server-side --field-manager=pod-42 -f - <<EOF
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: blue-network.pod.ipv4-10-0-0-0-26
spec:
  podNetworkRef: {kind: pod, name: blue-network}
  sliceSubnet: {family: IPv4, prefix: 10.0.0.0, prefixLength: 26}
  request:
  - name: pod-42
EOF
```

---

## 1. Deterministic Slices and Naming

An address space is tiled into fixed, disjoint slice blocks:
* **IPv4:** Fixed `/26` blocks ($2^6 = 64$ addresses per slice).
* **IPv6:** Fixed `/122` blocks ($2^6 = 64$ addresses per slice).

Because the slice size is constant and prefixes must be strictly aligned network addresses (host bits zero), **every IP belongs to exactly one slice**, and no two slices can ever overlap.

### The Canonical Naming Contract
An `IPSlice` object's name is a pure, canonical function of its block identity (`pkg/naming`):
```
<podNetworkRef.name>.<podNetworkRef.kind>[.<podNetworkRef.namespace>].<family>-<sanitized-prefix>-<prefixLength>
```
* Example IPv4: `blue-network.pod.ipv4-10-0-0-0-26`
* Example IPv6: `blue-network.pod.ipv6-fe80--40-122`

The `ValidatingAdmissionPolicy` (`deployment/validating-policy.yaml`) recomputes this string in CEL on create and **denies any object whose `metadata.name` does not match**. Two objects can never describe the same block because a second create collides on the name (`HTTP 409 AlreadyExists`).

---

## 2. Server-Side Apply (SSA) & Field Ownership

PizzIPAM uses Server-Side Apply (`types.ApplyPatchType` / `application/apply-patch+yaml`) to achieve **atomic create-or-merge**:

1. **Create-on-Update:** If the slice does not exist in `etcd`, SSA creates it. If it exists, SSA merges into it.
2. **Field-Level Ownership on Associative Lists:**
   `spec.request` is declared in the CRD schema as an associative map-list:
   ```yaml
   # +listType=map
   # +listMapKey=name
   Request []Request `json:"request,omitempty"`
   ```
   When a client applies with `--field-manager=pod-42`, SSA grants `pod-42` exclusive ownership of only its own list entry (`spec.request[name="pod-42"]`).
   * If another client applies with `--field-manager=pod-99`, SSA appends `pod-99` to the list without touching or clobbering `pod-42`.
3. **Atomic 1-by-1 Deletion:**
   When `pod-42` terminates, its CNI client sends an apply without `pod-42` (or deletes its field ownership). SSA removes only `pod-42` from `spec.request`, automatically releasing its IP offset back into the pool.

---

## 3. The Lifecycle of a Write inside `kube-apiserver`

When a client sends a Server-Side Apply request to `kube-apiserver`, here is the exact internal pipeline that executes:

```
[ Client Request ]
       │
       ▼
 1. SSA Merge in memory (patch.go)
       │
       ▼
 2. applyAdmission: MutatingAdmissionPolicy (CEL)
       ├─ Read surviving allocations from oldObject (anti-tamper)
       ├─ O(1) String scan: status.bitmap.indexOf("0") -> find free offset
       ├─ Update status.bitmap ("0" -> "1")
       └─ Append new entry to status.allocation
       │
       ▼
 3. CRD Schema Validations (types.go)
       └─ O(1) Check: size(status.allocation) == size(spec.request)
       │
       ▼
 4. etcd.OptimisticPut (Compare-And-Swap on resourceVersion)
       │
       ├─► [SUCCESS] ──► 5. ValidatingAdmissionPolicy (VAP)
       │                     └─ Structural & invariant checks
       │                     └─ Return HTTP 200/201 with allocated IP
       │
       └─► [CONFLICT] ──► 6. GuaranteedUpdate Internal Retry Loop
                             └─ Fetch fresh object from etcd
                             └─ RE-RUN MAP (Step 2) on new state!
                             └─ Re-attempt commit (Step 4)
```

### Step 2: The MutatingAdmissionPolicy (CEL Allocator)
The allocator runs purely inside `kube-apiserver`'s admission chain:
* **Anti-Tamper Guarantee:** Prior allocations are read from `oldObject.status`, never from the incoming request body. Any client-forged status or pre-declared offset is discarded.
* **Vectorized String Membership:** Requests and existing allocations are mapped to flat string lists (`reqNames`, `keptNames`), replacing thousands of closure evaluations with native string membership checks (`a.requestName in reqNames`).
* **Short-Circuited Deletion Filter:** On normal allocation writes (where no request was removed), the deletion filter is completely bypassed in zero nanoseconds (`size(kept) == size(existing) ? [] : ...`).
* **Sub-Microsecond Offset Selection:**
  Allocation state is tracked in `status.bitmap` (a 64-character string of `'0'` and `'1'`).
  * To allocate anywhere in the slice: `pick = baseBitmap.indexOf("0")`
  * To allocate inside a power-of-two window `[wlo, whi]`: `pick = baseBitmap.indexOf("0", wlo)`
  * Setting the bit on add: `newBitmap = baseBitmap.substring(0, pick) + "1" + baseBitmap.substring(pick + 1)`
  * Clearing the bit on 1-by-1 release: `clearedBitmap = oldBitmap.substring(0, freedOffset) + "0" + oldBitmap.substring(freedOffset + 1)`
  Finding and marking the free slot executes in **sub-microseconds** via native string operations without evaluating large loops over arrays.
* **IPv4 Address Rendering:**
  For IPv4, MAP computes the display address from precomputed base octets: `v4base + string(v4lastOctet + pick)`.
  For IPv6, the address is omitted from status (consumers derive it from `prefix + offset`).

### Step 3: Fast $O(1)$ CRD Schema Check
Before storage, the CRD schema executes `x-kubernetes-validations`:
```cel
size(self.status.allocation) == size(self.spec.request)
```
* If a free offset existed, MAP allocated it, so the sizes match $\implies$ admitted.
* If the slice was full (all 64 offsets occupied), MAP had no room and did not append an allocation $\implies$ size mismatch $\implies$ rejected with `"every spec.request must be allocated in status"`.

---

## 4. Concurrency & The `GuaranteedUpdate` Retry Loop

Because `etcd` is an optimistic concurrency control database, an entire `IPSlice` object is a single atomic row. If two clients apply to the same slice at the exact same millisecond:

1. **The Collision:**
   * Client A and Client B both read revision `100`.
   * Both evaluate MAP in memory.
   * Client A reaches `etcd` first: commits revision `101`.
   * Client B reaches `etcd`: expected `100`, but found `101` $\implies$ **Compare-And-Swap Conflict**.
2. **The API Server's Internal Retry:**
   In `k8s.io/apiserver/pkg/storage/etcd3/store.go`, Kubernetes runs `GuaranteedUpdate`:
   * It fetches revision `101` (which now contains Client A's allocation).
   * It re-applies Client B's patch.
   * **It re-runs `MutatingAdmissionPolicy` on the new version!**
3. **What MAP sees on the retry:**
   MAP reads the updated `status.bitmap` (where Client A's bit is now `'1'`).
   `indexOf("0")` skips Client A's offset and assigns the next free bit to Client B!
   Client B commits revision `102` cleanly.

Up to 5 internal retries happen transparently inside `kube-apiserver` without surfacing an error to the client.

---

## 5. ValidatingAdmissionPolicy (The Backstop)

Once a write successfully commits to `etcd`, `ValidatingAdmissionPolicy` runs as the final correctness guard:
* **Canonical Naming:** Verifies `metadata.name == canonicalName` (short-circuited on `UPDATE` since names are immutable).
* **Address Correctness:** Verifies IPv4 addresses strictly match `(prefix, offset)` and IPv6 allocations omit `address`.
* **1-at-a-Time Invariants:**
  * Adding $> 1$ request in a single write is rejected by CRD Rule 1.
  * Removing $> 1$ request in a single write is rejected by VAP Validation 7:
    `size(oldObject.spec.request.filter(r, !(r.name in currentRequests))) <= 1`

Because heavy $O(N^2)$ checks live in VAP rather than the CRD schema, **they run only once on commit, and never run during `GuaranteedUpdate` internal retry loops**.

---

## 6. Client-Side Subnet Walking (`pkg/allocator`)

When a consumer needs an IP from a larger pool (e.g. a `/24` = 4 slices, or a `/22` = 16 slices):

1. **Deterministic Slicing:**
   The client calculates the number of `/26` sub-slices: `numSlices = 1 << (sliceBits - subnetBits)`.
2. **Spreading with `WithOrder(Random)`:**
   Instead of all callers hitting `slice-0` first (creating a thundering herd), each client shuffles the slice indices into a random permutation.
3. **Hop on Contention:**
   If a client hits a slice that is crowded (returns `HTTP 409 Conflict` or `504 Timeout` after a quick local retry):
   * The client does not stay trapped on that slice.
   * It hops to the next slice in its permutation.
   * It uses **Full Jitter backoff** (`rand(0, window)`) between attempts to break synchronized waves.
4. **Caching Full Slices:**
   Once a slice returns `ErrSliceFull`, the client marks it in `fullSlices` so it never wastes another network call on that full slice during its allocation walk.
