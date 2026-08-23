# IPSlice

A controllerless IPAM: the API server allocates IP addresses at admission time.
The following rules are what make the allocation correct and unbreakable — every
one of them is load-bearing.

## Rules

1. **One object owns one block, and its name is deterministic and server-enforced —
   so a client can address a block without listing.** An IPSlice's name is a pure,
   canonical function of its block identity: `podNetworkRef` (name + kind + optional
   namespace) + `sliceSubnet.networkAddress`, concatenated as
   `<name>.<kind>[.<namespace>].<sliceNet>`. The string identity fields are
   constrained to DNS-1123 labels (lowercase, no dots) and `sliceNet` is a plain
   integer, so the result is a valid DNS-1123 subdomain and each identity maps to
   exactly one legal name.

   The identity deliberately does **not** include the slice *size* or the base
   pool. The slice size is a fixed system-wide constant (a `/26`, rule 4) and the
   `networkAddress` must be aligned to that grid, so an aligned `networkAddress`
   already names exactly one slice of the address space. Putting the size or the
   base in the name would let the *same* range of IPs be described by two different
   names (e.g. a `/26` and a `/25` at the same address, or the same slice under two
   different base pools) — two objects, overlapping ranges, the same IP handed out
   twice. Fixing the size and dropping the base is what keeps rule 2 (disjoint
   blocks) true without any cross-object check.

   This is **not a client contract — it is enforced**. `pkg/naming.Name()` is the
   single source of truth, and the `ValidatingAdmissionPolicy`
   (`deployment/validating-policy.yaml`) recomputes the same string in CEL and
   **denies any object whose `metadata.name` doesn't match its identity**. So a
   client cannot invent a name, and two objects can never describe the same block:
   the second create collides on the name (`AlreadyExists`). Combined with the
   identity fields being immutable (rule 9), the name always matches the content.
   (CEL has no hash function, so the enforced name is a readable concatenation
   rather than a digest — but it is just as deterministic.)

   The payoff for consumers: a client already knows the identity it wants — a pod
   network and a slice CIDR — so it computes the object name locally (with the same
   function the server enforces) and **adds its request with no prior GET or LIST**.
   A server-side apply creates the object if it doesn't exist yet and merges the
   request into the existing one if it does; because `spec.request` is a map-list
   keyed by `name`, independent clients each own their own entry and never clobber
   each other. Allocation is one round-trip, and there is no name to discover.

   This is what makes the "walk the slices" flow work. To get an IP for
   `podNetworkRef {name: abc, kind: blue-network}` somewhere in `192.168.0.0/24`,
   a client tries the first slice `192.168.0.0/26`: compute its name, apply a
   request, read back the allocated IP. If admission rejects it as *slice full*
   (rule 9's "every request allocated" check), move to the next slice
   `192.168.0.64/26`, compute its name, apply
   again — and so on until one accepts. No listing, no coordination: each attempt
   is a single self-describing apply.

2. **Blocks are disjoint — because they are a fixed partition.** Every slice is the
   same fixed size (a `/26`, rule 4) and its `networkAddress` must be aligned to
   that grid (`networkAddress % 64 == 0`, enforced in the schema). So the slices
   *tile* the address space: every IPv4 address falls in exactly one slice, and no
   two slices overlap. Combined with rule 1 (one object per slice, name-enforced),
   per-IP uniqueness stays *local* to a single object — no cross-object checks are
   ever needed to prove no two allocations share an address for a pod network in a
   CIDR. (A variable slice size would break this: nested/overlapping slices have
   different names, so the server couldn't stop them coexisting without listing.)

3. **All allocations for a block live in that one object.** Concurrent writers
   therefore collide on the object's `resourceVersion`, so etcd's compare-and-swap
   serializes them (the loser gets HTTP 409 and retries). This is the only
   atomicity primitive admission has, and it is sufficient precisely because
   everything for a block is in one object.

4. **Slice size is a fixed, small constant (`maxItems` caps the lists).** Every
   slice is a `/26` (`addressSpace 64`, all 64 allocatable — network and
   broadcast addresses included) — the schema pins
   `sliceSubnet.prefixLength == 26` and `addressSpace == 64` so the size can't vary
   (rule 2 depends on it), and `request`/`allocation` are capped by `maxItems 64`.
   The object stays far under etcd's ~1.5 MiB limit and the CEL enumeration is cheap
   and bounded; the cap is also what keeps the O(n²) cross-field rules inside the
   CRD cost budget. Changing the slice size means changing all of these in lockstep
   (the two pinned constants, the alignment modulus, `maxItems`, and the
   usable-range rule) — e.g. a `/25` would be `prefixLength 25`, `addressSpace 128`,
   alignment `% 128`, `maxItems 128`, which still fits the budget.

5. **The status subresource is OFF.** Admission runs on the main resource endpoint;
   a status subresource would strip the status written at admission. With it off,
   the policy can write `.status` inline and return the allocation to the caller.

6. **Status is computed from `oldObject`, never from the incoming object.** The
   `MutatingAdmissionPolicy` reads prior allocations from `oldObject` (the value
   already stored by the API server, itself produced by this policy) and runs on
   every write, always overwriting `.status`. So status is a pure function of
   `(oldObject.status, spec)` — a user can submit any `status` they like and it has
   zero effect. Nobody can forge or pin an allocation.

7. **IPs are stored as integers.** IPv4 fits in CEL's 64-bit ints, so free-address
   selection and the dotted `address` mirror are exact integer arithmetic — no
   parsing, no ambiguity.

8. **Requests are atomic and allocation is stable.** Adding/removing an entry in
   `spec.request` allocates/releases at admission and returns the IP to the caller
   immediately. A kept request never changes its IP (immutable once assigned); a
   removed request drops from `.status` (freeing its IP), and freed IPs are reused
   because free selection scans the whole block.

9. **The CRD schema is the backstop; the VAP covers only what it can't.**
   Everything expressible cheaply in the schema lives there (in `types.go`
   markers), not in a policy: field ranges and list caps, immutability of the
   block-identity fields (`self == oldSelf`), and object-level CEL
   (`x-kubernetes-validations`) for every request allocated, no orphan allocation,
   IPs in range, no duplicate IP, and no existing IP changed. Schema validation
   runs after the mutation and *always* runs (it can't be unbound like a policy),
   so it catches any bypass. Exactly one check does **not** fit: `address` matches
   `ip`. Its natural form (`address == string(ip…)`) is rejected by the CRD's
   *static* cost estimator, which sizes `string(int)` by the integer's max value
   (~4.29e9) rather than its digit count and so estimates it at >100× the budget.
   That one rule lives in a `ValidatingAdmissionPolicy`
   (`deployment/validating-policy.yaml`), where cost is enforced at *runtime* on
   the real octet values (0–255) and is trivial. Rule of thumb: put a check in the
   CRD if it fits the static budget; move it to a VAP only when it can't.

## Example

```yaml
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  # canonical name = <name>.<kind>.<sliceNet>  (no namespace here);
  # the VAP denies anything else.
  name: abc.blue-network.3232235520
spec:
  podNetworkRef: 
    kind: blue-network
    name: abc
  request:
  - name: my-request-0
  - name: my-request-1
  sliceSubnet: # 192.168.0.0/26 (slice size is a fixed /26)
    addressSpace: 64
    networkAddress: 3232235520
    prefixLength: 26
status:
  allocation:
  - address: 192.168.0.1
    ip: 3232235521
    requestName: my-request-0
  - address: 192.168.0.2
    ip: 3232235522
    requestName: my-request-1
```

## Requesting an IP without a GET or LIST

The client already knows the identity, so it computes the name (with `pkg/naming`,
the same function the server enforces) and applies only its own request entry
(server-side apply merges it into the map-list). It never has to discover or read
the object first:

```yaml
# apply.yaml — name = <name>.<kind>[.<namespace>].<sliceNet>
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.3232235520   # canonical; the VAP rejects any other
spec:
  podNetworkRef: {name: abc, kind: blue-network}
  sliceSubnet: {networkAddress: 3232235520, prefixLength: 26, addressSpace: 64}  # fixed /26
  request:
  - name: my-request-0      # only my entry; other clients' entries are untouched
```

```sh
$ kubectl apply --server-side --field-manager=my-request-0 -f apply.yaml
# -> creates the object (or merges into it) and returns it with my allocation in
#    status.allocation. Read status for my IP.
```

If the slice is full, admission rejects the apply:

```sh
The IPSlice "abc.blue-network.3232235520" is invalid: <root>: Invalid value:
"object": every spec.request must be allocated in status (the slice may be full)
```

Recompute the name for the next `/26` (`192.168.0.64/26`, `sliceSubnet
networkAddress: 3232235584` → name `abc.blue-network.3232235584`) and apply
again — repeat until one accepts.
