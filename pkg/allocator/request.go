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

package allocator

import (
	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/naming"
)

// Request defines the client request identity and configuration for an IP allocation or release.
// Implementations configure the underlying API Request and determine the unique
// request name used as the Server-Side Apply field manager.
type Request interface {
	// RequestName returns the unique request name used for SSA field management and matching.
	RequestName() string
	// Node returns the node name where the request originates, or empty if unset.
	Node() string
	// ApplyToRequest configures identity fields on the underlying API Request object.
	ApplyToRequest(req *v1alpha1.Request)
}

type nameRequest struct {
	name string
	node string
}

func (n nameRequest) RequestName() string { return n.name }
func (n nameRequest) Node() string        { return n.node }

func (n nameRequest) ApplyToRequest(req *v1alpha1.Request) {
	req.Name = n.name
	if n.node != "" {
		nodeCopy := n.node
		req.Node = &nodeCopy
	}
}

// ForNode returns a copy of the Request associated with the specified node name.
func (n nameRequest) ForNode(node string) nameRequest {
	n.node = node
	return n
}

// Named returns a Request identified by an explicit string name.
func Named(name string) nameRequest {
	return nameRequest{name: name}
}

type deviceRequest struct {
	ref  v1alpha1.ResourceClaimDeviceRef
	node string
}

func (d deviceRequest) RequestName() string {
	return naming.RequestNameForDevice(d.ref)
}

func (d deviceRequest) Node() string { return d.node }

func (d deviceRequest) ApplyToRequest(req *v1alpha1.Request) {
	req.Name = d.RequestName()
	refCopy := d.ref
	req.DeviceRef = &refCopy
	if d.node != "" {
		nodeCopy := d.node
		req.Node = &nodeCopy
	}
}

// ForNode returns a copy of the Request associated with the specified node name.
func (d deviceRequest) ForNode(node string) deviceRequest {
	d.node = node
	return d
}

// DeviceRef returns a Request derived from a DRA ResourceClaim allocated device.
// The request name is generated deterministically as claimNamespace.claimName.device.
func DeviceRef(ref v1alpha1.ResourceClaimDeviceRef) deviceRequest {
	return deviceRequest{ref: ref}
}
