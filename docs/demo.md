# PizzIPAM Walkthrough & Demo

This walkthrough demonstrates how PizzIPAM allocates, merges, validates, and releases IP addresses at admission time using Server-Side Apply (SSA), with zero controllers.

---

## 0. Install PizzIPAM

Install the CustomResourceDefinition and admission policies:

```bash
kubectl apply -f ./deployment
```

Verify that the CRD and admission policies are active:

```bash
kubectl get crd ipslices.multinetwork.networking.x-k8s.io
kubectl get mutatingadmissionpolicies,validatingadmissionpolicies
```

---

## 1. IPv4 Allocation (Single Round-Trip)

### Step 1: Allocate an IP for `pod-0`
A client sends a Server-Side Apply manifest carrying only its own request entry:

```bash
kubectl apply -o yaml --server-side --field-manager=pod-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: pod-0
EOF
```

**Output:** The API server synchronously allocates offset `0` and renders `192.168.0.0` inline:
```yaml
status:
  bitmap: "1000000000000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-0
    offset: 0
    address: 192.168.0.0
```

---

### Step 2: Concurrently Allocate an IP for `pod-1`
A second client applies to the same slice under its own field manager (`--field-manager=pod-1`):

```bash
kubectl apply -o yaml --server-side --field-manager=pod-1 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: pod-1
EOF
```

**Output:** SSA merges `pod-1` into `spec.request`, and MAP assigns offset `1` without touching `pod-0`:
```yaml
spec:
  request:
  - name: pod-0
  - name: pod-1
status:
  bitmap: "1100000000000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-0
    offset: 0
    address: 192.168.0.0
  - requestName: pod-1
    offset: 1
    address: 192.168.0.1
```

---

### Step 3: Window-Constrained Allocation (`pod-window`)
A client can request an allocation confined to an aligned power-of-two offset window (`offset` + `length`):

```bash
kubectl apply -o yaml --server-side --field-manager=pod-window -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: pod-window
    offset: 8
    length: 4   # offsets 8 .. 11
EOF
```

**Output:** MAP selects offset `8` (the lowest free offset in `[8..11]`):
```yaml
status:
  bitmap: "1100000010000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-0
    offset: 0
    address: 192.168.0.0
  - requestName: pod-1
    offset: 1
    address: 192.168.0.1
  - requestName: pod-window
    offset: 8
    address: 192.168.0.8
```

---

### Step 4: Release an Allocation (`pod-0`)
When `pod-0` terminates, it applies without its request entry under its field manager:

```bash
kubectl apply -o yaml --server-side --field-manager=pod-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
EOF
```

**Output:** `pod-0` drops from `spec.request`, its allocation is deleted, and bit 0 in `status.bitmap` returns to `'0'`. Surviving allocations (`pod-1` and `pod-window`) remain stable:
```yaml
status:
  bitmap: "0100000010000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-1
    offset: 1
    address: 192.168.0.1
  - requestName: pod-window
    offset: 8
    address: 192.168.0.8
```

---

### Step 5: Reuse the Freed Slot (`pod-reuse`)
When a new pod requests an IP, `status.bitmap.indexOf("0")` instantly locates offset `0`:

```bash
kubectl apply -o yaml --server-side --field-manager=pod-reuse -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: pod-reuse
EOF
```

**Output:** `pod-reuse` receives offset `0` (`192.168.0.0`):
```yaml
status:
  bitmap: "1100000010000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: pod-1
    offset: 1
    address: 192.168.0.1
  - requestName: pod-window
    offset: 8
    address: 192.168.0.8
  - requestName: pod-reuse
    offset: 0
    address: 192.168.0.0
```

---

## 2. IPv6 Allocation

IPv6 slices operate on fixed `/122` blocks ($2^6 = 64$ addresses). The allocator works in host offsets (`0..63`) and omits `address` from status so clients can derive the 128-bit address.

### Step 1: Base Slice (`fe80::/122`)
```bash
kubectl apply -o yaml --server-side --field-manager=v6-pod-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv6-fe80---122
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv6
    prefix: "fe80::"
    prefixLength: 122
  request:
  - name: v6-pod-0
EOF
```

**Output:** Offset `0` is assigned; consumer derives `fe80::0`:
```yaml
status:
  bitmap: "1000000000000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: v6-pod-0
    offset: 0
```

---

### Step 2: Non-Zero Aligned Slice (`fe80::40/122`)
Subsequent `/122` slices step by `0x40` (64 addresses): `::`, `::40`, `::80`, `::c0`:

```bash
kubectl apply -o yaml --server-side --field-manager=v6-pod-1 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv6-fe80--40-122
spec:
  podNetworkRef:
    kind: blue-network
    name: abc
  sliceSubnet:
    family: IPv6
    prefix: "fe80::40"
    prefixLength: 122
  request:
  - name: v6-pod-1
EOF
```

**Output:** Consumer derives concrete address `fe80::40` (`prefix + offset 0`):
```yaml
status:
  bitmap: "1000000000000000000000000000000000000000000000000000000000000000"
  allocation:
  - requestName: v6-pod-1
    offset: 0
```

---

## 3. Cleanup

Delete the demo slices:

```bash
kubectl delete ipslice abc.blue-network.ipv4-192-168-0-0-26
kubectl delete ipslice abc.blue-network.ipv6-fe80---122
kubectl delete ipslice abc.blue-network.ipv6-fe80--40-122
```
