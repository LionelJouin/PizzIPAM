# IPSlice

A controllerless IPAM: the API server allocates IP addresses at admission time.
The following rules are what make the allocation correct and unbreakable — every
one of them is load-bearing.

## Rules

1. **One object owns one block, and its name is deterministic and server-enforced —
   so a client can address a block without listing.** An IPSlice's name is a pure,
   canonical function of its block identity: `podNetworkRef` (name + kind + optional
   namespace) + `sliceSubnet` (family + prefix + prefixLength), concatenated as
   `<name>.<kind>[.<namespace>].<family-lower>-<sanitized-prefix>-<prefixLength>`.
   The string identity fields are constrained to DNS-1123 labels, and the slice
   token is built from the canonical network prefix with `.` and `:` replaced by
   `-` (so both `192.168.0.0` and `fe80::` become DNS-safe), so the result is a
   valid DNS-1123 subdomain and each identity maps to exactly one legal name.

   The identity does **not** encode the slice *size* independently of the family:
   the slice size is a fixed per-family constant (a `/26` for IPv4, a `/122` for
   IPv6 — rule 4), so once the family and the aligned prefix are fixed, exactly one
   slice of the address space is named. The prefix must be the network address
   (host bits zero, canonical form), so an aligned prefix already names exactly one
   slice. Letting the *same* range of IPs be described by two different names (e.g.
   a `/26` and a `/25` at the same address) would mean two objects, overlapping
   ranges, the same IP handed out twice. Fixing the size per family and requiring a
   canonical, aligned prefix is what keeps rule 2 (disjoint blocks) true without any
   cross-object check.

   This is **not a client contract — it is enforced**. `pkg/naming.Name()` is the
   single source of truth (it canonicalizes the prefix with `net/netip`), and the
   `ValidatingAdmissionPolicy` (`deployment/validating-policy.yaml`) recomputes the
   same string in CEL and **denies any object whose `metadata.name` doesn't match
   its identity**. So a client cannot invent a name, and two objects can never
   describe the same block: the second create collides on the name
   (`AlreadyExists`). Combined with the identity fields being immutable (rule 9),
   the name always matches the content. (CEL has no hash function, so the enforced
   name is a readable concatenation rather than a digest — but it is just as
   deterministic. CEL matches the client's canonical prefix because the CRD
   enforces `ip.isCanonical`, so the stored prefix is already canonical.)

   The payoff for consumers: a client already knows the identity it wants — a pod
   network and a slice prefix — so it computes the object name locally (with the
   same function the server enforces) and **adds its request with no prior GET or
   LIST**. A server-side apply creates the object if it doesn't exist yet and
   merges the request into the existing one if it does; because `spec.request` is a
   map-list keyed by `name`, independent clients each own their own entry and never
   clobber each other. Allocation is one round-trip, and there is no name to
   discover.

   This is what makes the "walk the slices" flow work. To get an IP for
   `podNetworkRef {name: abc, kind: blue-network}` somewhere in `192.168.0.0/24`,
   a client tries the first slice `192.168.0.0/26`: compute its name, apply a
   request, read back the allocated offset/address. If admission rejects it as
   *slice full* (rule 9's "every request allocated" check), move to the next slice
   `192.168.0.64/26`, compute its name, apply again — and so on until one accepts.
   No listing, no coordination: each attempt is a single self-describing apply.

2. **Blocks are disjoint — because they are a fixed partition.** Every slice is the
   same fixed size per family (a `/26` for IPv4, a `/122` for IPv6 — rule 4) and its
   `prefix` must be the network address of that block (host bits zero), enforced in
   the schema with
   `string(cidr(prefix + (family == 'IPv4' ? '/26' : '/122')).masked().ip()) == prefix`.
   (The prefix length is a literal, not `string(prefixLength)`: the CRD's static
   cost estimator sizes `string(int)` by the integer's max magnitude and would push
   this rule over budget — see rule 9 — and prefixLength is pinned per family
   anyway.) So the
   slices *tile* the address space: every address falls in exactly one slice, and no
   two slices overlap. Combined with rule 1 (one object per slice, name-enforced),
   per-IP uniqueness stays *local* to a single object — no cross-object checks are
   ever needed to prove no two allocations share an address. (A variable slice size
   would break this: nested/overlapping slices have different names, so the server
   couldn't stop them coexisting without listing.)

3. **All allocations for a block live in that one object.** Concurrent writers
   therefore collide on the object's `resourceVersion`, so etcd's compare-and-swap
   serializes them (the loser gets HTTP 409 and retries). This is the only
   atomicity primitive admission has, and it is sufficient precisely because
   everything for a block is in one object.

4. **Slice size is a fixed, small constant (`maxItems` caps the lists).** Every
   slice holds `64` addresses — a `/26` for IPv4, a `/122` for IPv6
   (all 64 allocatable, network and broadcast included). The schema pins the family
   ⇒ prefixLength pairing (`family == 'IPv4' ? prefixLength == 26 : prefixLength ==
   122`), which fixes the size — there is no separate `addressSpace` field; the size
   is implied by the pinned prefixLength and baked into the offset bounds (`0..63`)
   as literals — and `request`/`allocation` are capped by `maxItems 64`. The object stays far under
   etcd's ~1.5 MiB limit and the CEL enumeration is cheap and bounded (offsets
   `0..63`); the cap is also what keeps the O(n²) cross-field rules inside the CRD
   cost budget.

5. **The status subresource is OFF.** Admission runs on the main resource endpoint;
   a status subresource would strip the status written at admission. With it off,
   the policy can write `.status` inline and return the allocation to the caller.

6. **Status is computed from `oldObject`, never from the incoming object.** The
   `MutatingAdmissionPolicy` reads prior allocations from `oldObject` (the value
   already stored by the API server, itself produced by this policy) and runs on
   every write, always overwriting `.status`. So status is a pure function of
   `(oldObject.status, spec)` — a user can submit any `status` they like and it has
   zero effect. Nobody can forge or pin an allocation.

7. **Allocations are stored as host OFFSETS, not addresses — this is what makes the
   scheme family-agnostic.** CEL integers are signed 64-bit, so they cannot hold a
   128-bit IPv6 address. The allocator sidesteps that entirely: it reasons only
   about host offsets `0 .. 63`, which are small integers that fit
   int64 for *every* family. The network is an opaque canonical `prefix` string it
   never does math on. Free-address selection is exact integer arithmetic on the
   offset, identical for IPv4 and IPv6. The only family-specific step is rendering
   the display `address`: for IPv4 the allocator computes the dotted string (exact
   arithmetic on octets `0..255`); for IPv6 it stores the offset only and consumers
   derive the address from `(prefix, offset)`. A request narrows to an offset window
   with `offset` + `length` (length a power of two, offset a multiple of length),
   so there is no per-prefix subnet-size table — the old 33-branch `2^(32-prefix)`
   ternary is gone.

8. **Requests are atomic and allocation is stable.** Adding/removing an entry in
   `spec.request` allocates/releases at admission and returns the offset (and, for
   IPv4, the address) to the caller immediately. A kept request never changes its
   offset (immutable once assigned); a removed request drops from `.status` (freeing
   its offset), and freed offsets are reused because free selection scans the whole
   block.

9. **The CRD schema is the backstop; the VAP covers only what it can't.**
   Everything expressible cheaply in the schema lives there (in `types.go`
   markers), not in a policy: field ranges and list caps, immutability of the
   block-identity fields (`self == oldSelf`), the prefix validity / canonicality
   (`ip.isCanonical`) / family-match / alignment checks (via the IP and CIDR CEL
   libraries), and object-level CEL (`x-kubernetes-validations`) for every request
   allocated, no orphan allocation, offsets in range, no duplicate offset, no
   existing offset changed, and the allocated offset lying inside its request
   window. Schema validation runs after the mutation and *always* runs (it can't be
   unbound like a policy), so it catches any bypass. Two checks live in the
   `ValidatingAdmissionPolicy` (`deployment/validating-policy.yaml`) instead:
   (a) `metadata.name` equals the canonical name — the naming contract; and
   (b) for IPv4, `address` matches the render of `(prefix, offset)`. Check (b)'s
   natural form (`address == string(int…)`) is rejected by the CRD's *static* cost
   estimator, which sizes `string(int)` by the integer's max value rather than its
   digit count and so over-budgets it; in the VAP that cost is enforced at *runtime*
   on the real octet values (0–255) and is trivial. IPv6 stores no address, so (b)
   is skipped there. Rule of thumb: put a check in the CRD if it fits the static
   budget; move it to a VAP only when it can't.

## Example

```yaml
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  # canonical name = <name>.<kind>.<family-lower>-<sanitized-prefix>-<prefixLength>
  # (no namespace here); the VAP denies anything else.
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  request:
  - name: my-request-0
  - name: my-request-1
  sliceSubnet: # 192.168.0.0/26 (slice size is a fixed /26)
    family: IPv4
    prefix: 192.168.0.0
    prefixLength: 26
status:
  allocation:
  - requestName: my-request-0
    offset: 0
    address: 192.168.0.0
  - requestName: my-request-1
    offset: 1
    address: 192.168.0.1
```

The identical shape works for IPv6 — only the subnet and the stored allocation
differ (the offset is authoritative; there is no rendered `address`):

```yaml
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv6-fe80---122
spec:
  podNetworkRef: {kind: blue-network, name: abc}
  request:
  - name: my-request-0
  sliceSubnet: {family: IPv6, prefix: "fe80::", prefixLength: 122}
status:
  allocation:
  - requestName: my-request-0
    offset: 0            # consumers derive fe80::0 from (prefix, offset)
```

## Requesting an IP without a GET or LIST

The client already knows the identity, so it computes the name (with `pkg/naming`,
the same function the server enforces) and applies only its own request entry
(server-side apply merges it into the map-list). It never has to discover or read
the object first:

```yaml
# apply.yaml — name = <name>.<kind>[.<namespace>].<family>-<sanitized-prefix>-<prefixLength>
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26   # canonical; the VAP rejects any other
spec:
  podNetworkRef: {name: abc, kind: blue-network}
  sliceSubnet: {family: IPv4, prefix: 192.168.0.0, prefixLength: 26}  # fixed /26
  request:
  - name: my-request-0      # only my entry; other clients' entries are untouched
```

```sh
$ kubectl apply --server-side --field-manager=my-request-0 -f apply.yaml
# -> creates the object (or merges into it) and returns it with my allocation in
#    status.allocation. Read status for my offset (and, for IPv4, address).
```

If the slice is full, admission rejects the apply:

```sh
The IPSlice "abc.blue-network.ipv4-192-168-0-0-26" is invalid: <root>: Invalid
value: "object": every spec.request must be allocated in status (the slice may be full)
```

Recompute the name for the next `/26` (`192.168.0.64/26` → name
`abc.blue-network.ipv4-192-168-0-64-26`) and apply again — repeat until one
accepts.

## Performance characteristics (and one adversarial workload)

This design is optimized for **spread allocation**: many independent callers each
land in a *different* slice object. There every write is a lone writer on its own
object, admission runs once, and there is no contention — so throughput is high
and flat. In benchmarking against a real cluster this is roughly **~100× a
locking, single-pool-object allocator (whereabouts)**: e.g. ~1600 alloc/s
concurrent into a large fixed pool, versus ~10 alloc/s.

There is one workload where it does the opposite — **concurrently filling a pool
sized to just a few slices**:

- All allocations for a slice live in **one object** (this is what makes the
  no-controller, no-GET/LIST design work). Concurrent writers to that object
  serialize on the apiserver's optimistic-concurrency loop: the loser re-reads
  the fresh object and **re-runs the whole CEL admission**, then retries, until
  its write lands.
- So filling one slice of size `S` with `S` concurrent writers costs on the order
  of `S²` admission runs. Filling `M` addresses spread over `M/S` slices costs
  **≈ M·S** admission runs in total — linear in the *slice size*. With the fixed
  `S = 64` (a `/26`), a concurrent burst into a nearly-full small pool drives the
  apiserver CPU up and, once it saturates, individual applies exceed the client's
  request deadline and fail.

This is a deliberate trade, not a bug: the single-object-per-slice model is
exactly what buys the spread-case speed and the single-round-trip protocol. The
knob that trades between the two is the **slice size** (`S`): smaller slices mean
less concurrent-fill contention (and would allow pools smaller than a `/26`), but
more objects — which erodes the spread-case advantage. The size is fixed at `/26`
(IPv4) / `/122` (IPv6) here; see `apis/v1alpha1/types.go`.

Practical guidance:

- **Prefer a pool much larger than the number of IPs you allocate at once.** With
  ample free slices, callers spread naturally and never contend. This is the
  design's sweet spot.
- **Avoid a thundering-herd fill of a pool sized to a handful of slices.** If you
  must, stagger the burst (or accept that it degrades to roughly a serial fill).
- Pools **smaller than one slice** (e.g. a `/28`) are not supported: a slice is a
  fixed `/26`.
