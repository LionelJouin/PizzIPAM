# IPv6 spike — a family-agnostic, offset-based IPSlice

**Status: PROMOTED. This design shipped** — the offset-based, family-agnostic
model described here is now the real code in `apis/v1alpha1/types.go`,
`deployment/mutating-policy.yaml`, `deployment/validating-policy.yaml` and
`pkg/naming`. These files are kept as the design note that motivated the change;
the authoritative source of truth is the promoted code, not this folder. The
files below were the original self-contained proposal:

- `mutating-policy.yaml` — the allocator, rewritten to work on host offsets.
- `validating-policy.yaml` — the backstop (name + address render), family-aware.
- `examples.yaml` — an IPv4 and an IPv6 slice side by side.
- this README — the design, the proposed schema, and the **limitations** (the
  parts that hit a wall and need a decision).

## The problem IPv6 creates

CEL integers are signed `int64`. An IPv4 address fits (max ~4.29e9), so today the
allocator stores the absolute IP as an integer and does exact arithmetic on it:
pick a free `ip`, render `string(ip/16777216%256) + "." + ...`. An IPv6 address is
128-bit. It does **not** fit in `int64`, and neither does a `uint64` half
(`fe80::` alone is `0xFE80000000000000` ≈ 1.83e19, past `int64` max ~9.22e18). So
the "store the address as an integer" trick cannot carry over. This was the wall
the earlier `loUint64`/`hiUint64` schema hit.

## The insight: the allocator only needs the OFFSET

The allocator never actually needs the absolute address to do its job. It needs:

1. which host **offsets** (0 … addressSpace-1) are already taken, and
2. the lowest free offset inside the request's window.

An offset in a `/26` or `/122` is `0..63` — **always `int64`-safe, for every
address family.** The network itself is an opaque `prefix` string that CEL does no
arithmetic on. The absolute address is only ever needed for *display*, and display
is a string operation, not integer math on the full width.

So the model becomes:

- `sliceSubnet.family` — `IPv4` | `IPv6` (discriminator)
- `sliceSubnet.prefix` — the network address as an opaque **string**
  (`"192.168.0.0"`, `"fe80::"`)
- `sliceSubnet.prefixLength` — `26` for v4, `122` for v6 (both are 64 hosts)
- `sliceSubnet.addressSpace` — `64` (unchanged; the fixed partition size)
- the load-bearing number everywhere else is **`offset`** (`0..addressSpace-1`)

A request narrows with `(offset, length)` — a range **relative to the slice** —
instead of `(networkAddress, prefixLength)`. This is family-agnostic and, as a
bonus, **deletes the 33-branch `2^(32-prefix)` ternary** from both the allocator
and the CRD: `length` is supplied directly by the client and is small
(`≤ addressSpace`), so there is no power computation to enumerate.

## Proposed schema (Go types)

```go
type Subnet struct {
    // +kubebuilder:validation:Enum=IPv4;IPv6
    Family string `json:"family"`

    // Opaque network address string. CEL never does math on it; it is split/
    // concatenated for display only. MUST be canonical (see Limitations).
    // +kubebuilder:validation:MaxLength=45   // max IPv6 textual length
    Prefix string `json:"prefix"`

    // 26 for IPv4, 122 for IPv6 (both = 64 hosts).
    PrefixLength int32 `json:"prefixLength"`

    // Fixed partition size. Still 64.
    // +kubebuilder:validation:Minimum=1
    AddressSpace int32 `json:"addressSpace"`
}

type Request struct {
    // +kubebuilder:validation:MaxLength=63
    Name string `json:"name"`

    // Narrow to a sub-range of the slice: host offsets [offset, offset+length-1].
    // Both set together or both omitted. Immutable once set.
    // +optional
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=63
    Offset *int32 `json:"offset,omitempty"`
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=64
    Length *int32 `json:"length,omitempty"`
}

type Allocation struct {
    // +kubebuilder:validation:MaxLength=63
    RequestName string `json:"requestName"`

    // Host offset within the slice (the load-bearing number). No absolute `ip`.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=63
    Offset int32 `json:"offset"`

    // Rendered display address. Max IPv6 textual length.
    // +kubebuilder:validation:MaxLength=45
    Address string `json:"address"`
}
```

Note `Allocation` drops the `ip` field entirely — `offset` + the slice's `prefix`
fully determine the address, and keeping a third redundant encoding only weakens
anti-tamper (more surface to forge, more to keep consistent).

## Proposed CRD `x-kubernetes-validations` (readable form)

The offset model makes these **simpler** than the live IPv4 rules — no ternary,
no cross-slice disjointness check (offsets are inherently within the slice):

- **request window well-formed** (per request, when constrained):
  `!has(r.offset) || (r.offset % r.length == 0 && r.offset + r.length <= self.spec.sliceSubnet.addressSpace)`
  — plus `has(r.offset) == has(r.length)` and `length` a power of two
  (`length in [1,2,4,8,16,32,64]`, a 7-element list, not a 33-branch ternary).
- **every request allocated / no orphan allocation** — unchanged in spirit.
- **offset in range**: `a.offset >= 0 && a.offset < self.spec.sliceSubnet.addressSpace`.
- **no duplicate offset**: `size(self.status.allocation.filter(x, x.offset == a.offset)) == 1`.
- **existing offset never changes** (transition rule) — same as today, on `offset`.
- **allocated offset within its request window**:
  `!has(r.offset) || (a.offset >= r.offset && a.offset < r.offset + r.length)`.
- **family/size consistency**:
  `self.family == 'IPv4' ? self.prefixLength == 26 : self.prefixLength == 122`,
  and `self.addressSpace == 64`.

The **disjoint-subnet root rule is gone**: a request window is expressed in slice
offsets, so it cannot be disjoint from the slice by construction — the CRD only
checks `offset + length <= addressSpace`. That removes the single most expensive
rule in the live CRD.

## What changes vs the live IPv4 pipeline

| Aspect | Live (IPv4-only) | Spike (unified) |
|---|---|---|
| Address storage | `ip int64` (absolute) | `offset int32` (relative) |
| Request narrowing | `networkAddress`+`prefixLength` | `offset`+`length` |
| `2^(32-prefix)` ternary | in CRD **and** MAP | **gone** |
| Disjoint-subnet root rule | present (most expensive) | **gone** |
| Address render | dotted-quad in CEL | v4 dotted / v6 hex, branched on `family` |
| Name identity token | integer `networkAddress` | sanitized `prefix` string |

Everything that made the IPv4 design correct is preserved: one object per block,
status rebuilt from `oldObject` every write (anti-tamper), one allocation per
write, release-on-remove, CAS via `resourceVersion`.

## Limitations — the parts that hit a wall

These are the real findings. They need a decision, not more code.

1. **IPv6 uniqueness depends on a canonical prefix string.** For IPv4 the identity
   token was an **integer** — one representation per network, so two objects could
   never name the same block. A prefix **string** has many spellings for one
   network (`fe80::` = `fe80:0:0:0:0:0:0:0` = `FE80::`). The name is only airtight
   if the prefix is canonical. Mitigation: `pkg/naming.Name()` canonicalizes with
   `net/netip` (Go) and the VAP recomputes the *same* token — but the VAP cannot
   itself *prove* canonicality in CEL, so a hand-crafted non-canonical prefix
   would get a self-consistent-but-different name and could coexist with the
   canonical object. **This is a genuine regression from the IPv4 integer's
   airtight uniqueness.** Options: (a) accept it (clients always use `pkg/naming`);
   (b) constrain `prefix` with a strict canonical-form regex in the CRD;
   (c) keep an integer identity for v4 and only use the string form for v6.

2. **CEL can only render IPv6 when the trailing hextet is zero.** The allocator
   renders v6 as `prefix + hexOffset[offset]` (e.g. `"fe80::" + "3f"` →
   `fe80::3f`). This is correct only when the `/122` network's last hextet is `0`
   (prefix ends in `::`). For a prefix like `2001:db8::40/122`, offset 5 must be
   `2001:db8::45` — that needs **hex addition** on the last hextet (`0x40 + 5`),
   which CEL cannot do cheaply (no hex parse, and a per-slice lookup table can't be
   a static literal). Options: (a) restrict v6 slices to `::`-aligned prefixes
   (documented constraint); (b) push v6 rendering out of CEL entirely — the client
   computes `address`, the VAP validates structurally; (c) store the last-hextet
   base as an int and render with a 64-entry `base+offset` table generated per
   family (still awkward). **(b) is the honest production answer**: offset is the
   allocator's truth; the display string is advisory and can be client-rendered +
   VAP-checked for length/family only.

3. **This spike assumes the CEL strings extension** (`split`, `replace`,
   `lowerAscii`, `int(string)`). Kubernetes enables it for admission CEL, but the
   exact set/behavior must be confirmed on the target 1.36 apiserver.

4. **CEL runtime behavior is NOT verified.** There is no offline CEL cost/eval
   oracle in this repo and the standing constraint is no cluster. So the YAML here
   is parse-checked and the arithmetic is reasoned through, but **cost-budget fit
   and actual allocation behavior must be confirmed by an e2e run on a cluster**
   before any of this replaces the live files.

## What I verified offline

- All three YAML files parse.
- The offset arithmetic is `int64`-safe for every family (max value is
  `addressSpace-1 = 63`).
- The CRD rule set loses the 33-branch ternary and the disjoint-subnet rule, so it
  is strictly cheaper than the live CRD (which already fits the budget) — modulo
  the new `split`/`replace` string ops in the VAP, whose runtime cost is on tiny
  strings.

## Suggested next step

Decide Limitation #1 (v6 uniqueness) and #2 (v6 rendering) first — they shape the
schema. My recommendation: **offset is canonical truth; render v6 client-side and
have the VAP validate only length+family** (Limitation #2 option b), and **use a
strict canonical-prefix regex** (Limitation #1 option b). If we accept that, I can
promote this to real `apis/` types + regenerated CRD and an e2e case, keeping IPv4
behavior identical, and we validate the whole thing on a kind cluster in CI.
