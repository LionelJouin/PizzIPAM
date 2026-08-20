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
// +kubebuilder:validation:XValidation:rule="!has(self.spec.request) || self.spec.request.all(r, has(self.status) && has(self.status.allocation) && self.status.allocation.exists(a, a.requestName == r.name))",message="every spec.request must be allocated in status (the slice may be full)"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, has(self.spec.request) && self.spec.request.exists(r, r.name == a.requestName))",message="status.allocation has an entry with no matching spec.request"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, a.ip >= self.spec.sliceSubnet.networkAddress + 1 && a.ip <= self.spec.sliceSubnet.networkAddress + self.spec.sliceSubnet.addressSpace - 2)",message="an allocated IP is outside the slice usable range"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || self.status.allocation.all(a, size(self.status.allocation.filter(x, x.ip == a.ip)) == 1)",message="duplicate IP in status.allocation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.allocation) || oldSelf.status.allocation.all(o, !(has(self.spec.request) && self.spec.request.exists(r, r.name == o.requestName)) || (has(self.status) && has(self.status.allocation) && self.status.allocation.exists(a, a.requestName == o.requestName && a.ip == o.ip)))",message="an existing allocation IP cannot change (only released by removing its request)"

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

	// BaseSubnet is the base subnet for this IP slice.
	// This field is part of the block identity encoded in the object name; it is immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="baseSubnet is immutable; create a new IPSlice for a different block"
	BaseSubnet Subnet `json:"baseSubnet"`

	// SliceSubnet is the subnet for this IP slice.
	// This field is part of the block identity encoded in the object name; it is immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sliceSubnet is immutable; create a new IPSlice for a different block"
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
type PodNetworkRef struct {
	// Kind identifies the NetworkKind responsible for the pod network.
	// +required
	// +kubebuilder:validation:MaxLength=253
	Kind string `json:"kind"`

	// Name identifies the pod network object.
	// +required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace identifies the namespace of the pod network object.
	// Optional if the pod network object is a non-namespace object.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace *string `json:"namespace,omitempty"`
}

type Request struct {
	// Name identifies the request.
	// +required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
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
	// +required
	// +kubebuilder:validation:Minimum=0
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
