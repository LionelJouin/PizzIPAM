# IPSlice

A controllerless IPAM: the API server allocates IP addresses at admission time.
The following rules are what make the allocation correct and unbreakable — every
one of them is load-bearing.

## Rules

1. **One object owns one block.** An IPSlice's name is deterministic:
   `PodNetworkRef.Name + PodNetworkRef.Kind + PodNetworkRef.Namespace (optional) + baseSubnet.networkAddress + baseSubnet.addressSpace + sliceSubnet.networkAddress`.
   Two objects can never describe the same block (the name would collide), and
   those fields become immutable (changing one is a different object).

2. **Blocks are disjoint.** Each IPSlice owns its own `sliceSubnet`. Because slices
   don't overlap and each block has exactly one object, per-IP uniqueness stays
   *local* to a single object — no cross-object checks are ever needed to prove no
   two allocations share an address for a pod network in a CIDR.

3. **All allocations for a block live in that one object.** Concurrent writers
   therefore collide on the object's `resourceVersion`, so etcd's compare-and-swap
   serializes them (the loser gets HTTP 409 and retries). This is the only
   atomicity primitive admission has, and it is sufficient precisely because
   everything for a block is in one object.

4. **Slice size is small and bounded (`maxItems` caps the lists).** A single slice
   is a small block — the example uses a /26 (`addressSpace 64`, 62 usable) — and
   `request`/`allocation` are capped by `maxItems`, so the object stays far under
   etcd's ~1.5 MiB limit and the CEL enumeration is cheap and bounded. The cap is
   what keeps the O(n²) cross-field rules inside the CRD cost budget; keep them in
   step (a /25 would need `addressSpace 128` and `maxItems 128`, which still fits).

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
  name: blue-network
spec:
  baseSubnet: # 192.168.0.0/24
    addressSpace: 256
    networkAddress: 3232235520
    prefixLength: 24
  podNetworkRef: 
    kind: blue-network
    name: abc
  request:
  - name: my-request-0
  - name: my-request-1
  sliceSubnet: # 192.168.0.0/26
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
