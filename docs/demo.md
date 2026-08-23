# Demo

kubectl apply -f ./deployment

kubectl apply -f examples/example.yaml -o yaml

## IPv4

```yaml
kubectl apply -o yaml -f - <<EOF
---
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    name: abc
    kind: blue-network
  sliceSubnet: # 192.168.0.0/26 (slice size is a fixed /26 by design)
    family: IPv4
    prefix: "192.168.0.0" # opaque network string; the allocator does no math on it
    prefixLength: 26
EOF
```

```yaml
kubectl apply -o yaml --server-side --field-manager=request-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    name: abc
    kind: blue-network
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: request-0
EOF
```

```yaml
kubectl apply -o yaml --server-side --field-manager=request-1 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    name: abc
    kind: blue-network
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
  request:
  - name: request-1
EOF
```

```yaml
kubectl apply -o yaml --server-side --field-manager=request-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.ipv4-192-168-0-0-26
spec:
  podNetworkRef:
    name: abc
    kind: blue-network
  sliceSubnet:
    family: IPv4
    prefix: "192.168.0.0"
    prefixLength: 26
EOF
```