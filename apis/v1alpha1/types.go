/*
Copyright (c) 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:shortName=ipsl,scope=Cluster
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// Object-level validations (cross-field spec<->status). These run at schema
// validation time, after the MutatingAdmissionPolicy has filled status.
//
// Everything expressible cheaply in the CRD lives here. The one exception is the
// address<->ip consistency check: its natural form (a.address == string(a.ip...))
// is rejected by the CRD's STATIC cost estimator, which sizes string(int) by the
// int's max VALUE (~4.29e9) and so estimates it at >100x the budget. That rule
// lives in the ValidatingAdmissionPolicy instead (deployment/validating-policy.yaml),
// where cost is enforced at RUNTIME on the real values (octets 0-255) and is cheap.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.request) || self.spec.request.all(r, has(self.status) && has(self.status.allocation) && self.status.allocation.exists(a, a.requestName == r.name))",message="every spec.request must be allocated in status (add new requests one at a time, or the slice is full)"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, has(self.spec.request) && self.spec.request.exists(r, r.name == a.requestName))",message="status.allocation has an entry with no matching spec.request"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, a.ip >= self.spec.sliceSubnet.networkAddress + 1 && a.ip <= self.spec.sliceSubnet.networkAddress + self.spec.sliceSubnet.addressSpace - 2)",message="an allocated IP is outside the slice usable range"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, size(self.status.allocation.filter(x, x.ip == a.ip)) == 1)",message="duplicate IP in status.allocation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.allocation) || oldSelf.status.allocation.all(o, !(has(self.spec.request) && self.spec.request.exists(r, r.name == o.requestName)) || (has(self.status) && has(self.status.allocation) && self.status.allocation.exists(a, a.requestName == o.requestName && a.ip == o.ip)))",message="an existing allocation IP cannot change (only released by removing its request)"
// A constrained request's subnet must be COMPATIBLE with this slice: one must
// contain the other (their ranges overlap). A disjoint subnet is rejected. The
// subnet size is 2^(32-prefixLength), enumerated as a ternary because CEL has no
// shift/pow. This is pure integer arithmetic, so it fits the CRD cost budget.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.request) || self.spec.request.all(r, !has(r.networkAddress) || (r.networkAddress < self.spec.sliceSubnet.networkAddress + self.spec.sliceSubnet.addressSpace && self.spec.sliceSubnet.networkAddress < r.networkAddress + (r.prefixLength == 32 ? 1 : r.prefixLength == 31 ? 2 : r.prefixLength == 30 ? 4 : r.prefixLength == 29 ? 8 : r.prefixLength == 28 ? 16 : r.prefixLength == 27 ? 32 : r.prefixLength == 26 ? 64 : r.prefixLength == 25 ? 128 : r.prefixLength == 24 ? 256 : r.prefixLength == 23 ? 512 : r.prefixLength == 22 ? 1024 : r.prefixLength == 21 ? 2048 : r.prefixLength == 20 ? 4096 : r.prefixLength == 19 ? 8192 : r.prefixLength == 18 ? 16384 : r.prefixLength == 17 ? 32768 : r.prefixLength == 16 ? 65536 : r.prefixLength == 15 ? 131072 : r.prefixLength == 14 ? 262144 : r.prefixLength == 13 ? 524288 : r.prefixLength == 12 ? 1048576 : r.prefixLength == 11 ? 2097152 : r.prefixLength == 10 ? 4194304 : r.prefixLength == 9 ? 8388608 : r.prefixLength == 8 ? 16777216 : r.prefixLength == 7 ? 33554432 : r.prefixLength == 6 ? 67108864 : r.prefixLength == 5 ? 134217728 : r.prefixLength == 4 ? 268435456 : r.prefixLength == 3 ? 536870912 : r.prefixLength == 2 ? 1073741824 : r.prefixLength == 1 ? 2147483648 : 4294967296)))",message="a request subnet is disjoint from this slice; the slice and the request subnet must contain one another"
// Each allocated IP must fall inside its request's subnet (when the request set
// one). Together with the usable-range rule above, this pins the IP to the
// (slice AND request) intersection, so a tampered status cannot place an IP
// outside what was asked for.
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, self.spec.request.exists(r, r.name == a.requestName && (!has(r.networkAddress) || (a.ip >= r.networkAddress && a.ip < r.networkAddress + (r.prefixLength == 32 ? 1 : r.prefixLength == 31 ? 2 : r.prefixLength == 30 ? 4 : r.prefixLength == 29 ? 8 : r.prefixLength == 28 ? 16 : r.prefixLength == 27 ? 32 : r.prefixLength == 26 ? 64 : r.prefixLength == 25 ? 128 : r.prefixLength == 24 ? 256 : r.prefixLength == 23 ? 512 : r.prefixLength == 22 ? 1024 : r.prefixLength == 21 ? 2048 : r.prefixLength == 20 ? 4096 : r.prefixLength == 19 ? 8192 : r.prefixLength == 18 ? 16384 : r.prefixLength == 17 ? 32768 : r.prefixLength == 16 ? 65536 : r.prefixLength == 15 ? 131072 : r.prefixLength == 14 ? 262144 : r.prefixLength == 13 ? 524288 : r.prefixLength == 12 ? 1048576 : r.prefixLength == 11 ? 2097152 : r.prefixLength == 10 ? 4194304 : r.prefixLength == 9 ? 8388608 : r.prefixLength == 8 ? 16777216 : r.prefixLength == 7 ? 33554432 : r.prefixLength == 6 ? 67108864 : r.prefixLength == 5 ? 134217728 : r.prefixLength == 4 ? 268435456 : r.prefixLength == 3 ? 536870912 : r.prefixLength == 2 ? 1073741824 : r.prefixLength == 1 ? 2147483648 : 4294967296)))))",message="an allocated IP is outside its request's subnet"

// IPSlice describes a slice of IP addresses.
type IPSlice struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// spec is the desired state of the IPSlice.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	Spec IPSliceSpec `json:"spec,omitempty"`
	// status is the current state of the IPSlice.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	Status IPSliceStatus `json:"status,omitempty"`
}

// IPSliceSpec describes the desired state of the IPSlice.
type IPSliceSpec struct {
	// PodNetworkRef identifies the pod network that this IP slice is associated with.
	// This field is part of the block identity encoded in the object name; it is immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="podNetworkRef is immutable; create a new IPSlice for a different block"
	PodNetworkRef PodNetworkRef `json:"podNetworkRef"`

	// Request is the list of IP requests for this IP slice.
	// +listType=map
	// +listMapKey=name
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Request []Request `json:"request,omitempty"`

	// SliceSubnet is the subnet for this IP slice.
	//
	// The slice size is a fixed system-wide constant: every slice is a /26
	// (prefixLength 26, addressSpace 64). This is what makes the blocks a true
	// partition — with a fixed size and an aligned networkAddress, every IPv4
	// address belongs to exactly one slice, so disjointness holds WITHOUT any
	// cross-object check (rule 2). networkAddress alone (plus podNetworkRef) is
	// therefore the block identity encoded in the name; it is immutable.
	//
	// If you ever change the slice size, change all four in lockstep: the two
	// constants below, the alignment modulus, and the usable-range root rule.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sliceSubnet is immutable; create a new IPSlice for a different block"
	// +kubebuilder:validation:XValidation:rule="self.prefixLength == 26",message="slice size is fixed: sliceSubnet.prefixLength must be 26 (a /26)"
	// +kubebuilder:validation:XValidation:rule="self.addressSpace == 64",message="slice size is fixed: sliceSubnet.addressSpace must be 64 (a /26)"
	// +kubebuilder:validation:XValidation:rule="self.networkAddress % 64 == 0",message="sliceSubnet.networkAddress must be aligned to the /26 grid (a multiple of 64)"
	SliceSubnet Subnet `json:"sliceSubnet"`
}

// IPSliceStatus describes the observed state of the IPSlice.
type IPSliceStatus struct {
	// Allocation is the list of IP allocations for this IP slice.
	// +listType=map
	// +listMapKey=requestName
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Allocation []Allocation `json:"allocation,omitempty"`
}

// PodNetworkRef identifies a specific pod network instance.
//
// Its fields (together with sliceSubnet.networkAddress) form the block identity
// that the object's metadata.name is derived from and enforced against by the
// ValidatingAdmissionPolicy. They are therefore constrained to DNS-1123 labels:
// lowercase, no dots, <=63 chars. That keeps the derived dotted name a valid
// DNS-1123 subdomain and makes the identity -> name mapping unambiguous (a '.'
// inside a component could otherwise collide two different identities).
type PodNetworkRef struct {
	// Kind identifies the NetworkKind responsible for the pod network.
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Kind string `json:"kind"`

	// Name identifies the pod network object.
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Namespace identifies the namespace of the pod network object.
	// Optional if the pod network object is a non-namespace object.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace *string `json:"namespace,omitempty"`
}

// Request asks the allocator for one IP address.
//
// By default the IP is taken from anywhere in the slice. A request MAY narrow
// where the IP comes from by naming a subnet (networkAddress + prefixLength): the
// allocator then picks a free IP from the intersection of that subnet and this
// slice. The two fields are set together or not at all, and the subnet must be
// aligned. The subnet only has to be COMPATIBLE with the slice — one must contain
// the other (a tighter subnet inside the slice, or a broader subnet that contains
// it); a disjoint subnet is rejected (see the root "request subnet" rule).
//
// +kubebuilder:validation:XValidation:rule="has(self.networkAddress) == has(self.prefixLength)",message="networkAddress and prefixLength must be set together (a request's subnet), or both omitted (allocate from anywhere in the slice)"
// +kubebuilder:validation:XValidation:rule="!has(self.networkAddress) || self.networkAddress % (self.prefixLength == 32 ? 1 : self.prefixLength == 31 ? 2 : self.prefixLength == 30 ? 4 : self.prefixLength == 29 ? 8 : self.prefixLength == 28 ? 16 : self.prefixLength == 27 ? 32 : self.prefixLength == 26 ? 64 : self.prefixLength == 25 ? 128 : self.prefixLength == 24 ? 256 : self.prefixLength == 23 ? 512 : self.prefixLength == 22 ? 1024 : self.prefixLength == 21 ? 2048 : self.prefixLength == 20 ? 4096 : self.prefixLength == 19 ? 8192 : self.prefixLength == 18 ? 16384 : self.prefixLength == 17 ? 32768 : self.prefixLength == 16 ? 65536 : self.prefixLength == 15 ? 131072 : self.prefixLength == 14 ? 262144 : self.prefixLength == 13 ? 524288 : self.prefixLength == 12 ? 1048576 : self.prefixLength == 11 ? 2097152 : self.prefixLength == 10 ? 4194304 : self.prefixLength == 9 ? 8388608 : self.prefixLength == 8 ? 16777216 : self.prefixLength == 7 ? 33554432 : self.prefixLength == 6 ? 67108864 : self.prefixLength == 5 ? 134217728 : self.prefixLength == 4 ? 268435456 : self.prefixLength == 3 ? 536870912 : self.prefixLength == 2 ? 1073741824 : self.prefixLength == 1 ? 2147483648 : 4294967296) == 0",message="a request's networkAddress must be aligned to its prefixLength (a multiple of the subnet size)"
type Request struct {
	// Name identifies the request.
	// +required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// NetworkAddress is the network address of the subnet to allocate from,
	// as an integer (int(192.168.0.4) == 3232235524). Optional: set together
	// with prefixLength to constrain the allocation to a subnet, or omit both
	// to allocate from anywhere in the slice. Immutable once set.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a request's networkAddress is immutable; remove the request and add a new one to change its subnet"
	NetworkAddress *int64 `json:"networkAddress,omitempty"`

	// PrefixLength is the prefix length of the subnet to allocate from.
	// Optional: set together with networkAddress. Immutable once set.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a request's prefixLength is immutable; remove the request and add a new one to change its subnet"
	PrefixLength *int32 `json:"prefixLength,omitempty"`
}

type Allocation struct {
	// Name identifies from which request this allocation was made.
	// +required
	// +kubebuilder:validation:MaxLength=63
	RequestName string `json:"requestName"`

	// IP is the allocated IP address.
	// Bounded to the IPv4 range so CEL cost estimation of string(ip) stays cheap.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	IP int64 `json:"ip"`

	// Address is the allocated IP address in string format.
	// Max "255.255.255.255" == 15 chars.
	// +required
	// +kubebuilder:validation:MaxLength=15
	Address string `json:"address"`
}

type Subnet struct {
	// NetworkAddress is the network address of the subnet.
	// Bounded to the IPv4 range; it is part of the block identity encoded in the
	// object name, so it must stay a bounded, stringifiable integer.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	NetworkAddress int64 `json:"networkAddress"`

	// PrefixLength is the prefix length of the subnet.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=32
	PrefixLength int32 `json:"prefixLength"`

	// AddressSpace is the address space of the subnet.
	// For IPv4, the address space is 2^(32 - prefixLength).
	// For IPv6, the address space is 2^(128 - prefixLength).
	// +required
	// +kubebuilder:validation:Minimum=1
	AddressSpace int32 `json:"addressSpace"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPSliceList is a list of IPSlice resources.
// +k8s:deepcopy-gen=true
type IPSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []IPSlice `json:"items"`
}
