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

package allocator_test

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"testing"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/allocator"
	versioned "github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned"
	"github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned/fake"
	"github.com/lioneljouin/pizzipam/pkg/naming"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clienttesting "k8s.io/client-go/testing"
)

// sliceHosts mirrors the fixed slice size in pkg/allocator: every slice holds 64
// addresses, so offsets run 0..63 and a 65th distinct request cannot fit.
const sliceHosts = 64

var ipsliceGVR = schema.GroupVersionResource{
	Group:    v1alpha1.GroupVersion.Group,
	Version:  v1alpha1.GroupVersion.Version,
	Resource: "ipslices",
}

// newFakeClient returns a fake clientset, optionally prefilled with objects,
// that stands in for the apiserver's admission-time allocation. The generated
// fake tracker stores the applied object as-is and never fills status.allocation
// (that is the CRD's CEL job), so Allocate would find no allocation for its
// request. This reactor simulates the server: it keeps any allocations the
// stored slice already carries (occupied offsets stay stable, and an existing
// request name keeps its offset -- idempotency), and gives every not-yet
// allocated request in the apply body the lowest free host offset.
func newFakeClient(objects ...runtime.Object) versioned.Interface {
	cs := fake.NewSimpleClientset(objects...)
	cs.PrependReactor("patch", "ipslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchAction)
		if !ok {
			return false, nil, nil
		}
		applied := &v1alpha1.IPSlice{}
		if err := json.Unmarshal(pa.GetPatch(), applied); err != nil {
			return true, nil, err
		}

		result := applied.DeepCopy()
		result.Status.Allocation = nil
		// Seed with the stored slice's allocations so a prefilled object keeps
		// its occupied offsets across this apply.
		if existing, err := cs.Tracker().Get(ipsliceGVR, pa.GetNamespace(), pa.GetName()); err == nil {
			result.Status.Allocation = existing.(*v1alpha1.IPSlice).DeepCopy().Status.Allocation
		}

		occupied := map[int32]bool{}
		allocated := map[string]bool{}
		for _, a := range result.Status.Allocation {
			occupied[a.Offset] = true
			allocated[a.RequestName] = true
		}
		for _, r := range applied.Spec.Request {
			if allocated[r.Name] {
				continue
			}
			off := int32(0)
			for occupied[off] {
				off++
			}
			if off >= sliceHosts {
				// No free offset left: reject the way the CRD's CEL rule does, so
				// Allocate maps it to ErrSliceFull and walks on to the next slice.
				return true, nil, apierrors.NewInvalid(
					schema.GroupKind{Group: ipsliceGVR.Group, Kind: "IPSlice"},
					applied.Name,
					field.ErrorList{field.Invalid(
						field.NewPath("spec", "request"),
						r.Name,
						"every spec.request must be allocated in status",
					)},
				)
			}
			occupied[off] = true
			allocated[r.Name] = true
			result.Status.Allocation = append(result.Status.Allocation, v1alpha1.Allocation{
				RequestName: r.Name,
				Offset:      off,
			})
		}
		if _, err := cs.Tracker().Get(ipsliceGVR, pa.GetNamespace(), pa.GetName()); err != nil {
			_ = cs.Tracker().Create(ipsliceGVR, result, pa.GetNamespace())
		} else {
			_ = cs.Tracker().Update(ipsliceGVR, result, pa.GetNamespace())
		}
		return true, result, nil
	})
	return cs
}

// sliceName mirrors the (ref, subnet) -> object name mapping Allocate performs,
// so a test can prefill the very object a later Allocate call will manipulate.
func sliceName(ref *v1alpha1.PodNetworkRef, subnet netip.Prefix) string {
	prefix := subnet.Masked().Addr().Unmap()
	family := "IPv4"
	if !prefix.Is4() {
		family = "IPv6"
	}
	return naming.Name(v1alpha1.IPSliceSpec{
		PodNetworkRef: *ref,
		SliceSubnet: v1alpha1.Subnet{
			Family:       family,
			Prefix:       prefix.String(),
			PrefixLength: int32(subnet.Bits()),
		},
	})
}

// prefilledSlice builds an IPSlice already carrying the given allocations, named
// so that Allocate(ref, subnet, ...) resolves to it.
func prefilledSlice(ref *v1alpha1.PodNetworkRef, subnet netip.Prefix, allocs ...v1alpha1.Allocation) *v1alpha1.IPSlice {
	return &v1alpha1.IPSlice{
		ObjectMeta: metav1.ObjectMeta{Name: sliceName(ref, subnet)},
		Status:     v1alpha1.IPSliceStatus{Allocation: allocs},
	}
}

// fullSlice builds a slice with every offset (0..63) already allocated, so any
// further request against it is rejected as full.
func fullSlice(ref *v1alpha1.PodNetworkRef, subnet netip.Prefix) *v1alpha1.IPSlice {
	allocs := make([]v1alpha1.Allocation, 0, sliceHosts)
	for off := range int32(sliceHosts) {
		allocs = append(allocs, v1alpha1.Allocation{
			RequestName: fmt.Sprintf("filler-%d", off),
			Offset:      off,
		})
	}
	return prefilledSlice(ref, subnet, allocs...)
}

func TestAllocate(t *testing.T) {
	ref := &v1alpha1.PodNetworkRef{Name: "test-network"}

	tests := []struct {
		name        string
		client      versioned.Interface
		ref         *v1alpha1.PodNetworkRef
		subnet      netip.Prefix
		requestName string
		want        netip.Addr
		wantErr     bool
	}{
		{
			name:        "IPv4 /26",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/26"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.0"),
			wantErr:     false,
		},
		{
			name:        "IPv4 /24",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/24"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.0"),
			wantErr:     false,
		},
		{
			name:        "IPv6 /120",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("2001:db8::/120"),
			requestName: "test-request",
			want:        netip.MustParseAddr("2001:db8::"),
			wantErr:     false,
		},
		{
			// The slice already holds an allocation at offset 0, so the new
			// request must be given the next free offset (1 -> 192.168.0.1).
			name: "prefilled slice, next free offset",
			client: newFakeClient(prefilledSlice(ref, netip.MustParsePrefix("192.168.0.0/26"),
				v1alpha1.Allocation{RequestName: "other-request", Offset: 0})),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/26"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.1"),
			wantErr:     false,
		},
		{
			// Re-requesting an already-allocated name is idempotent: the slice
			// keeps the existing offset (2 -> 192.168.0.2).
			name: "prefilled slice, idempotent re-request",
			client: newFakeClient(prefilledSlice(ref, netip.MustParsePrefix("192.168.0.0/26"),
				v1alpha1.Allocation{RequestName: "test-request", Offset: 2})),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/26"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.2"),
			wantErr:     false,
		},
		{
			// A /25 (128 addresses, .0-.127) is larger than one slice, so Allocate
			// walks its two /26 sub-slices (192.168.0.0/26 then 192.168.0.64/26).
			// Both empty, so it lands in the first at offset 0.
			name:        "IPv4 /25 walk, first sub-slice",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/25"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.0"),
			wantErr:     false,
		},
		{
			// First /26 sub-slice is full, so the walk moves on and allocates
			// from the second (192.168.0.64/26) at offset 0.
			name:        "IPv4 /25 walk, spills into second sub-slice",
			client:      newFakeClient(fullSlice(ref, netip.MustParsePrefix("192.168.0.0/26"))),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/25"),
			requestName: "test-request",
			want:        netip.MustParseAddr("192.168.0.64"),
			wantErr:     false,
		},
		{
			// Both /26 sub-slices of the /25 are full: no address left anywhere.
			name: "IPv4 /25 walk, all sub-slices full",
			client: newFakeClient(
				fullSlice(ref, netip.MustParsePrefix("192.168.0.0/26")),
				fullSlice(ref, netip.MustParsePrefix("192.168.0.64/26")),
			),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/25"),
			requestName: "test-request",
			wantErr:     true,
		},
		{
			// IPv6 /121 walk: first /122 (2001:db8::/122) is full, so it spills
			// into the second /122 (2001:db8::40/122).
			name:   "IPv6 /121 walk, spills into second sub-slice (::40)",
			client: newFakeClient(fullSlice(ref, netip.MustParsePrefix("2001:db8::/122"))),
			ref:    ref,
			subnet: netip.MustParsePrefix("2001:db8::/121"),
			requestName: "test-request",
			want:        netip.MustParseAddr("2001:db8::40"),
			wantErr:     false,
		},
		{
			// IPv6 /120 walk: first and second /122 are full, spills into third (2001:db8::80/122).
			name: "IPv6 /120 walk, spills into third sub-slice (::80)",
			client: newFakeClient(
				fullSlice(ref, netip.MustParsePrefix("2001:db8::/122")),
				fullSlice(ref, netip.MustParsePrefix("2001:db8::40/122")),
			),
			ref:         ref,
			subnet:      netip.MustParsePrefix("2001:db8::/120"),
			requestName: "test-request",
			want:        netip.MustParseAddr("2001:db8::80"),
			wantErr:     false,
		},
		{
			// IPv6 /120 walk: first 3 are full, spills into fourth (2001:db8::c0/122).
			name: "IPv6 /120 walk, spills into fourth sub-slice (::c0)",
			client: newFakeClient(
				fullSlice(ref, netip.MustParsePrefix("2001:db8::/122")),
				fullSlice(ref, netip.MustParsePrefix("2001:db8::40/122")),
				fullSlice(ref, netip.MustParsePrefix("2001:db8::80/122")),
			),
			ref:         ref,
			subnet:      netip.MustParsePrefix("2001:db8::/120"),
			requestName: "test-request",
			want:        netip.MustParseAddr("2001:db8::c0"),
			wantErr:     false,
		},
		{
			// Non-zero offset in an IPv6 non-zero sub-slice (::40 with offset 5 -> ::45).
			name: "IPv6 ::40 slice with offset 5",
			client: newFakeClient(prefilledSlice(ref, netip.MustParsePrefix("2001:db8::40/122"),
				v1alpha1.Allocation{RequestName: "r0", Offset: 0},
				v1alpha1.Allocation{RequestName: "r1", Offset: 1},
				v1alpha1.Allocation{RequestName: "r2", Offset: 2},
				v1alpha1.Allocation{RequestName: "r3", Offset: 3},
				v1alpha1.Allocation{RequestName: "r4", Offset: 4},
			)),
			ref:         ref,
			subnet:      netip.MustParsePrefix("2001:db8::40/122"),
			requestName: "test-request",
			want:        netip.MustParseAddr("2001:db8::45"),
			wantErr:     false,
		},
		{
			// A subnet smaller than the fixed slice size is rejected outright.
			name:        "subnet smaller than slice",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/27"),
			requestName: "test-request",
			wantErr:     true,
		},
		{
			name:        "nil ref",
			client:      newFakeClient(),
			ref:         nil,
			subnet:      netip.MustParsePrefix("192.168.0.0/26"),
			requestName: "test-request",
			wantErr:     true,
		},
		{
			name:        "empty request name",
			client:      newFakeClient(),
			ref:         ref,
			subnet:      netip.MustParsePrefix("192.168.0.0/26"),
			requestName: "",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotErr := allocator.Allocate(t.Context(), tt.client, tt.ref, tt.subnet, tt.requestName)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("Allocate() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("Allocate() succeeded unexpectedly")
			}
			if got != tt.want {
				t.Errorf("Allocate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAllocateRandomOrder covers WithOrder(Random): it must stay correct (only
// ever land in a real sub-slice), still spill past a full slice, and actually
// vary which sub-slice it tries first rather than always packing the lowest one.
func TestAllocateRandomOrder(t *testing.T) {
	ref := &v1alpha1.PodNetworkRef{Name: "test-network"}
	subnet := netip.MustParsePrefix("192.168.0.0/25") // two /26 sub-slices: .0 and .64
	first := netip.MustParseAddr("192.168.0.0")
	second := netip.MustParseAddr("192.168.0.64")

	t.Run("stays within the subnet and varies the first slice", func(t *testing.T) {
		seen := map[netip.Addr]bool{}
		// A range of seeds gives a deterministic, non-flaky sample of orders.
		for seed := range uint64(32) {
			rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // test-only determinism
			got, err := allocator.Allocate(t.Context(), newFakeClient(), ref, subnet, "test-request",
				allocator.WithOrder(allocator.Random), allocator.WithRand(rng))
			if err != nil {
				t.Fatalf("Allocate() failed: %v", err)
			}
			if got != first && got != second {
				t.Fatalf("Allocate() = %v, want one of %v / %v", got, first, second)
			}
			seen[got] = true
		}
		if !seen[first] || !seen[second] {
			t.Errorf("random order never varied the first slice: only saw %v", seen)
		}
	})

	t.Run("still spills past a full slice", func(t *testing.T) {
		// The first sub-slice is full, so whatever order it tries them in, the
		// only free address is in the second sub-slice.
		client := newFakeClient(fullSlice(ref, netip.MustParsePrefix("192.168.0.0/26")))
		rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test-only determinism
		got, err := allocator.Allocate(t.Context(), client, ref, subnet, "test-request",
			allocator.WithOrder(allocator.Random), allocator.WithRand(rng))
		if err != nil {
			t.Fatalf("Allocate() failed: %v", err)
		}
		if got != second {
			t.Errorf("Allocate() = %v, want %v", got, second)
		}
	})
}

func TestIPv6AddressDerivation(t *testing.T) {
	tests := []struct {
		prefix string
		offset int32
		want   string
	}{
		{"2001:db8::", 0, "2001:db8::"},
		{"2001:db8::", 5, "2001:db8::5"},
		{"2001:db8::", 63, "2001:db8::3f"},
		{"2001:db8::40", 0, "2001:db8::40"},
		{"2001:db8::40", 5, "2001:db8::45"},
		{"2001:db8::40", 63, "2001:db8::7f"},
		{"2001:db8::80", 0, "2001:db8::80"},
		{"2001:db8::80", 63, "2001:db8::bf"},
		{"2001:db8::c0", 0, "2001:db8::c0"},
		{"2001:db8::c0", 63, "2001:db8::ff"},
		{"2001:db8::100", 0, "2001:db8::100"},
		{"2001:db8::100", 63, "2001:db8::13f"},
	}

	ref := &v1alpha1.PodNetworkRef{Name: "test-network"}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s+offset%d", tt.prefix, tt.offset), func(t *testing.T) {
			subnet := netip.MustParsePrefix(fmt.Sprintf("%s/122", tt.prefix))
			var allocations []v1alpha1.Allocation
			for i := int32(0); i < tt.offset; i++ {
				allocations = append(allocations, v1alpha1.Allocation{
					RequestName: fmt.Sprintf("req-%d", i),
					Offset:      i,
				})
			}
			client := newFakeClient(prefilledSlice(ref, subnet, allocations...))
			got, err := allocator.Allocate(t.Context(), client, ref, subnet, "my-request")
			if err != nil {
				t.Fatalf("Allocate() failed: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("Allocate() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRelease(t *testing.T) {
	ref := &v1alpha1.PodNetworkRef{Name: "test-network"}
	subnet := netip.MustParsePrefix("192.168.0.0/26")

	t.Run("release with WithRequestName", func(t *testing.T) {
		ip := netip.MustParseAddr("192.168.0.5")
		client := newFakeClient(prefilledSlice(ref, subnet, v1alpha1.Allocation{
			RequestName: "pod-5",
			Offset:      5,
		}))

		err := allocator.Release(t.Context(), client, ref, ip, allocator.WithRequestName("pod-5"))
		if err != nil {
			t.Fatalf("Release() failed: %v", err)
		}
	})

	t.Run("release without request name looks up allocation", func(t *testing.T) {
		ip := netip.MustParseAddr("192.168.0.5")
		client := newFakeClient(prefilledSlice(ref, subnet, v1alpha1.Allocation{
			RequestName: "pod-5",
			Offset:      5,
		}))

		err := allocator.Release(t.Context(), client, ref, ip)
		if err != nil {
			t.Fatalf("Release() failed: %v", err)
		}
	})

	t.Run("release IPv6 address", func(t *testing.T) {
		v6Ref := &v1alpha1.PodNetworkRef{Name: "v6-network"}
		v6Subnet := netip.MustParsePrefix("2001:db8::40/122")
		ip := netip.MustParseAddr("2001:db8::45")
		client := newFakeClient(prefilledSlice(v6Ref, v6Subnet, v1alpha1.Allocation{
			RequestName: "v6-pod",
			Offset:      5,
		}))

		err := allocator.Release(t.Context(), client, v6Ref, ip, allocator.WithRequestName("v6-pod"))
		if err != nil {
			t.Fatalf("Release() failed: %v", err)
		}
	})

	t.Run("invalid inputs", func(t *testing.T) {
		client := newFakeClient()
		ip := netip.MustParseAddr("192.168.0.1")

		if err := allocator.Release(t.Context(), client, nil, ip); err == nil {
			t.Error("Release() with nil ref expected error")
		}
		if err := allocator.Release(t.Context(), client, ref, netip.Addr{}); err == nil {
			t.Error("Release() with invalid IP expected error")
		}
	})
}
