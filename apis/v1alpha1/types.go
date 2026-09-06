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
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.podNetworkRef.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.podNetworkRef.kind`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.podNetworkRef.namespace`,priority=1
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.sliceSubnet.family`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.sliceSubnet.prefix`
// +kubebuilder:printcolumn:name="PrefixLength",type=integer,JSONPath=`.spec.sliceSubnet.prefixLength`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// Object-level validations (cross-field spec<->status). These run at schema
// validation time, after the MutatingAdmissionPolicy has filled status.
//
// The allocator reasons entirely in host OFFSETS (0 .. 63), which are small
// integers that fit CEL's int64 for EVERY address family (IPv4 and IPv6).
// The network is an opaque canonical `prefix` STRING that CEL never does math on;
// the IP/CIDR CEL libraries (isIP, ip.isCanonical, family, cidr().masked()) check
// it cheaply. This is why the whole thing works for IPv6 despite CEL being 64-bit:
// nothing here ever holds a 128-bit address.
//
// 1. every request is allocated; 2. no orphan allocation; 3. offset in range;
// 4. an existing offset never changes (release-only);
// 5. a constrained request's window fits inside the slice; 6. each allocation's
// offset lies inside its request's window. Rules 5-6 replace the old
// 2^(32-prefix) ternary and disjoint-subnet check entirely -- an offset window is
// expressed in slice offsets, so it cannot be disjoint from the slice.
// 1. every request is allocated in status (size match signals whether allocation succeeded or slice is full).
// 2. no orphan allocation.
// (Detailed O(N^2) semantic invariant checks live in ValidatingAdmissionPolicy so they run once on commit
// rather than being re-evaluated on every internal GuaranteedUpdate CAS retry).
// +kubebuilder:validation:XValidation:rule="!has(self.spec.request) || (has(self.status) && has(self.status.allocation) && size(self.status.allocation) == size(self.spec.request))",message="every spec.request must be allocated in status (the slice may be full)"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || size(self.status.allocation) <= (has(self.spec.request) ? size(self.spec.request) : 0)",message="status.allocation has an entry with no matching spec.request"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, a.offset >= 0 && a.offset < 64)",message="an allocated offset is outside the slice range"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.request) || self.spec.request.all(r, !has(r.offset) || r.offset + r.length <= 64)",message="a request's offset window must fit inside the slice (offset + length <= 64)"

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
	// The slice size is a fixed system-wide constant: every slice holds 64
	// addresses -- a /26 for IPv4, a /122 for IPv6. The size is not a field: it is
	// implied by the pinned prefixLength (below) and baked into the offset bounds
	// (0..63) as literals. This is what makes the blocks a true partition: with a
	// fixed size and a network-aligned prefix, every address belongs to exactly one
	// slice, so disjointness holds WITHOUT any cross-object check (rule 2). The
	// prefix (plus podNetworkRef) is the block identity encoded in the name; the
	// whole subnet is immutable.
	//
	// Uniqueness across families relies on the prefix being CANONICAL: unlike an
	// integer, a text IP has many spellings for one network. ip.isCanonical()
	// enforces the single canonical form in CEL, so a canonical prefix maps to
	// exactly one name (see pkg/naming.Name and the VAP).
	//
	// If you ever change the slice size, change these in lockstep: the pinned
	// prefixLengths, the offset bounds (0..63 on Request/Allocation and the "< 64"
	// root rules), and MaxItems on the lists.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sliceSubnet is immutable; create a new IPSlice for a different block"
	// +kubebuilder:validation:XValidation:rule="oldSelf != null || isIP(self.prefix)",message="sliceSubnet.prefix must be a valid IP address"
	// +kubebuilder:validation:XValidation:rule="oldSelf != null || !isIP(self.prefix) || ip.isCanonical(self.prefix)",message="sliceSubnet.prefix must be in canonical form (e.g. lowercase, compressed IPv6)"
	// +kubebuilder:validation:XValidation:rule="oldSelf != null || !isIP(self.prefix) || ip(self.prefix).family() == (self.family == 'IPv4' ? 4 : 6)",message="sliceSubnet.prefix family must match sliceSubnet.family"
	// +kubebuilder:validation:XValidation:rule="oldSelf != null || (self.family == 'IPv4' ? self.prefixLength == 26 : self.prefixLength == 122)",message="slice size is fixed: prefixLength must be 26 for IPv4 or 122 for IPv6 (both are 64 addresses)"
	// The prefix length is a family-branched string LITERAL ('/26' or '/122'), not
	// string(self.prefixLength): the CRD's static cost estimator sizes string(int)
	// by the integer's max magnitude, which inflates the concatenated CIDR string
	// and pushes this rule >100x over budget. prefixLength is already pinned per
	// family (rule above), so the literal is exact and keeps the check in the CRD
	// (where it always runs and can't be unbound) instead of a VAP.
	// +kubebuilder:validation:XValidation:rule="oldSelf != null || !isIP(self.prefix) || string(cidr(self.prefix + (self.family == 'IPv4' ? '/26' : '/122')).masked().ip()) == self.prefix",message="sliceSubnet.prefix must be the network address (host bits zero) aligned to prefixLength"
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
// By default the address is taken from anywhere in the slice. A request MAY narrow
// where it comes from with an OFFSET WINDOW: offset (the first host offset within
// the slice) plus length (the number of addresses). The allocator then picks the
// lowest free offset in [offset, offset+length-1]. The two fields are set together
// or not at all; the window must be aligned (offset % length == 0), a power of two
// in size, and fit inside the slice (checked by the root rules). Offsets are
// family-agnostic small integers, so this works identically for IPv4 and IPv6.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.offset) && !has(self.length)) || (has(self.offset) && has(self.length) && self.offset % self.length == 0 && (self.length == 1 || self.length == 2 || self.length == 4 || self.length == 8 || self.length == 16 || self.length == 32 || self.length == 64))",message="a request's window must specify offset and power-of-two length (1, 2, 4, 8, 16, 32 or 64) with aligned offset, or omit both"
type Request struct {
	// Name identifies the request.
	// +required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Offset is the first host offset (within the slice) of the window to allocate
	// from, 0 .. 63. Optional: set together with length to constrain
	// the allocation to a window, or omit both to allocate from anywhere in the
	// slice. Immutable once set.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a request's offset is immutable; remove the request and add a new one to change its window"
	Offset *int32 `json:"offset,omitempty"`

	// Length is the size of the window to allocate from (a power of two).
	// Optional: set together with offset. Immutable once set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a request's length is immutable; remove the request and add a new one to change its window"
	Length *int32 `json:"length,omitempty"`
}

type Allocation struct {
	// Name identifies from which request this allocation was made.
	// +required
	// +kubebuilder:validation:MaxLength=63
	RequestName string `json:"requestName"`

	// Offset is the allocated host offset within the slice, 0 .. 63.
	// This is the load-bearing value: the concrete address is (prefix + offset).
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=63
	Offset int32 `json:"offset"`

	// Address is the allocated IP address in string format, rendered by the
	// allocator. It is set for IPv4 (which CEL can render from prefix + offset with
	// exact integer arithmetic) and omitted for IPv6 (which CEL cannot render:
	// there is no 128-bit arithmetic). For IPv6, consumers derive the address from
	// the slice prefix and this offset. Max IPv6 text length is 45 chars.
	// +optional
	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address,omitempty"`
}

type Subnet struct {
	// Family is the IP family of the slice: "IPv4" or "IPv6".
	// +required
	// +kubebuilder:validation:Enum=IPv4;IPv6
	Family string `json:"family"`

	// Prefix is the network address of the subnet, as a canonical IP string
	// (e.g. "192.168.0.0" or "fe80::"). It is part of the block identity encoded in
	// the object name. CEL does no arithmetic on it; the IP/CIDR CEL libraries
	// validate it. Must be canonical (ip.isCanonical) so the identity -> name
	// mapping is unambiguous. Max IPv6 text length is 45 chars.
	// +required
	// +kubebuilder:validation:MaxLength=45
	Prefix string `json:"prefix"`

	// PrefixLength is the prefix length of the subnet: 26 for IPv4, 122 for IPv6
	// (both cover 64 addresses). This is what fixes the slice size; there is no
	// separate addressSpace field (the size is implied and baked into the offset
	// bounds 0..63).
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	PrefixLength int32 `json:"prefixLength"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPSliceList is a list of IPSlice resources.
// +k8s:deepcopy-gen=true
type IPSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []IPSlice `json:"items"`
}
