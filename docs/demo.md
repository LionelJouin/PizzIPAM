# Demo

kubectl apply -f ./deployment

kubectl apply -f examples/example.yaml -o yaml


```sh
# IPv4
IFS=. read -r a b c d <<< "192.168.0.0"; echo $(( (a<<24)+(b<<16)+(c<<8)+d ))
# IPv6
python3 -c 'import ipaddress; print(int(ipaddress.IPv6Address("2001:db8::1")))'
```

```yaml
kubectl apply -o yaml -f - <<EOF
---
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.3232235520
spec:
  podNetworkRef:
    name: abc
    kind: blue-network
  sliceSubnet: # 192.168.0.0/26 (slice size is a fixed /26)
    networkAddress: 3232235520
    prefixLength: 26
    addressSpace: 64 # 2^(32-26)
EOF
```

```sh
kubectl apply -o yaml --server-side --field-manager=request-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.3232235520
spec:
  podNetworkRef: {name: abc, kind: blue-network}
  sliceSubnet: {networkAddress: 3232235520, prefixLength: 26, addressSpace: 64}
  request:
  - name: request-0
EOF
```

```sh
kubectl apply -o yaml --server-side --field-manager=request-1 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.3232235520
spec:
  podNetworkRef: {name: abc, kind: blue-network}
  sliceSubnet: {networkAddress: 3232235520, prefixLength: 26, addressSpace: 64}
  request:
  - name: request-1
EOF
```

```sh
kubectl apply -o yaml --server-side --field-manager=request-0 -f - <<'EOF'
apiVersion: multinetwork.networking.x-k8s.io/v1alpha1
kind: IPSlice
metadata:
  name: abc.blue-network.3232235520
spec:
  podNetworkRef: {name: abc, kind: blue-network}
  sliceSubnet: {networkAddress: 3232235520, prefixLength: 26, addressSpace: 64}
EOF
```