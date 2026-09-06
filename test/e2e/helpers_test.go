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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/naming"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

const (
	// The IPv4 slice is 192.168.0.0/26; the IPv6 slice is fe80::/122. Both hold 64
	// addresses, so the allocator works in host OFFSETS 0..63 for either family.
	v4Prefix       = "192.168.0.0"
	v6Prefix       = "fe80::"
	sliceAddresses = int32(64)

	offsetLo = int32(0)                  // first allocatable offset (network address included)
	offsetHi = sliceAddresses - int32(1) // last allocatable offset (broadcast included) -> 63
)

func i32(v int32) *int32 { return &v }

// v4Address mirrors the allocator's (prefix, offset) -> address render for the
// IPv4 slice, so the test can assert the two status fields stay consistent.
func v4Address(offset int32) string {
	return fmt.Sprintf("192.168.0.%d", offset)
}

func newSlice(requests ...v1alpha1.Request) *v1alpha1.IPSlice {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: v4Prefix, PrefixLength: 26},
		Request:       requests,
	}
	// The name is server-enforced: it MUST be naming.Name(spec) or the
	// ValidatingAdmissionPolicy denies the write. GenerateName would fail.
	return &v1alpha1.IPSlice{
		ObjectMeta: metav1.ObjectMeta{Name: naming.Name(spec)},
		Spec:       spec,
	}
}

// newSliceV6 builds an IPv6 slice (fe80::/122). Its allocations carry the offset
// only -- CEL cannot render a 128-bit address, so status.allocation[].address is
// left empty and consumers derive the address from (prefix, offset).
func newSliceV6(requests ...v1alpha1.Request) *v1alpha1.IPSlice {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{Family: "IPv6", Prefix: v6Prefix, PrefixLength: 122},
		Request:       requests,
	}
	return &v1alpha1.IPSlice{
		ObjectMeta: metav1.ObjectMeta{Name: naming.Name(spec)},
		Spec:       spec,
	}
}

// applyRequest performs the recommended single-round-trip allocation from
// docs/demo.md: a server-side apply that carries ONLY this caller's own request
// entry, under its own field manager. Because spec.request is a map-list keyed by
// name, each field manager owns just its own entry and never clobbers another's,
// so independent clients can allocate on the same slice with no prior GET.
func applyRequest(ctx context.Context, name, requestName string) error {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: v4Prefix, PrefixLength: 26},
		Request:       []v1alpha1.Request{{Name: requestName}},
	}
	obj := &v1alpha1.IPSlice{
		TypeMeta:   metav1.TypeMeta{APIVersion: "multinetwork.networking.x-k8s.io/v1alpha1", Kind: "IPSlice"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	force := true
	backoff := wait.Backoff{Steps: 20, Duration: 10 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Cap: 2 * time.Second}
	return retry.RetryOnConflict(backoff, func() error {
		_, err := concurrentClient.MultinetworkV1alpha1().IPSlices().Patch(
			ctx, name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: requestName, Force: &force},
		)
		return err
	})
}
