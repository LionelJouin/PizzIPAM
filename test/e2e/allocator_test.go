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
	"errors"
	"fmt"
	"net/netip"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/allocator"
	"github.com/lioneljouin/pizzipam/pkg/naming"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tests that exercise the public client-side helper (pkg/allocator.Allocate)
// directly against a live Kubernetes cluster with MutatingAdmissionPolicy active.
var _ = Describe("Allocator client helper (pkg/allocator)", func() {
	var cleanSlices []string

	AfterEach(func(ctx SpecContext) {
		for _, name := range cleanSlices {
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, name, metav1.DeleteOptions{})
		}
		cleanSlices = nil
	})

	trackSubnetSlices := func(ref *v1alpha1.PodNetworkRef, subnet netip.Prefix) {
		base := subnet.Masked().Addr().Unmap()
		family := "IPv4"
		sliceBits := 26
		if !base.Is4() {
			family = "IPv6"
			sliceBits = 122
		}
		shift := sliceBits - subnet.Bits()
		numSlices := uint64(1) << uint(shift)
		for i := uint64(0); i < numSlices; i++ {
			prefix := addAddr(base, i*uint64(sliceAddresses))
			name := naming.Name(v1alpha1.IPSliceSpec{
				PodNetworkRef: *ref,
				SliceSubnet:   v1alpha1.Subnet{Family: family, Prefix: prefix.String(), PrefixLength: int32(sliceBits)},
			})
			cleanSlices = append(cleanSlices, name)
		}
	}

	It("drives multi-slice allocation and idempotency against the live cluster", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "live-walk"}
		subnet := netip.MustParsePrefix("192.168.10.0/25") // holds two /26 sub-slices: .0 and .64
		trackSubnetSlices(ref, subnet)

		By("allocating the first address from the subnet")
		addr1, err := allocator.Allocate(ctx, client, ref, subnet, "pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1).To(Equal(netip.MustParseAddr("192.168.10.0")))

		By("idempotently re-requesting the same name")
		addr1Again, err := allocator.Allocate(ctx, client, ref, subnet, "pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1Again).To(Equal(addr1), "idempotent request must return the same address")

		By("allocating a second address")
		addr2, err := allocator.Allocate(ctx, client, ref, subnet, "pod-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr2).To(Equal(netip.MustParseAddr("192.168.10.1")))
	})

	It("allocates concrete IPv6 addresses across multi-slice subnets via allocator.Allocate", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "v6-walk"}
		subnet := netip.MustParsePrefix("2001:db8:10::/121") // holds two /122 sub-slices: :: and ::40
		trackSubnetSlices(ref, subnet)

		By("allocating the first IPv6 address")
		addr1, err := allocator.Allocate(ctx, client, ref, subnet, "v6-pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1).To(Equal(netip.MustParseAddr("2001:db8:10::")))

		By("idempotently re-requesting the same IPv6 name")
		addr1Again, err := allocator.Allocate(ctx, client, ref, subnet, "v6-pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1Again).To(Equal(addr1), "idempotent IPv6 request must return the same address")

		By("allocating a second IPv6 address")
		addr2, err := allocator.Allocate(ctx, client, ref, subnet, "v6-pod-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr2).To(Equal(netip.MustParseAddr("2001:db8:10::1")))
	})

	It("spreads concurrent callers across a multi-slice subnet with WithOrder(Random)", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "rand-walk"}
		subnet := netip.MustParsePrefix("192.168.20.0/25") // two /26 sub-slices: .0 and .64
		trackSubnetSlices(ref, subnet)

		count := 16
		var wg sync.WaitGroup
		addrs := make([]netip.Addr, count)
		errs := make([]error, count)

		for i := 0; i < count; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				defer GinkgoRecover()
				addrs[idx], errs[idx] = allocator.Allocate(ctx, concurrentClient, ref, subnet,
					fmt.Sprintf("rand-pod-%d", idx), allocator.WithOrder(allocator.Random))
			}(i)
		}
		wg.Wait()

		seen := map[netip.Addr]bool{}
		for i, err := range errs {
			Expect(err).NotTo(HaveOccurred(), "concurrent allocation %d must succeed", i)
			Expect(addrs[i].IsValid()).To(BeTrue())
			Expect(seen[addrs[i]]).To(BeFalse(), "duplicate address %v under concurrent walk", addrs[i])
			seen[addrs[i]] = true
		}
		Expect(seen).To(HaveLen(count))
	})

	It("allocates and releases an IP via allocator.Allocate and allocator.Release on live cluster", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "alloc-rel"}
		subnet := netip.MustParsePrefix("192.168.50.0/26")
		trackSubnetSlices(ref, subnet)

		By("allocating an IP")
		addr, err := allocator.Allocate(ctx, client, ref, subnet, "pod-rel-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr).To(Equal(netip.MustParseAddr("192.168.50.0")))

		By("releasing the IP using allocator.Release with WithRequestName (0-GET path)")
		err = allocator.Release(ctx, client, ref, addr, allocator.WithRequestName("pod-rel-1"))
		Expect(err).NotTo(HaveOccurred())

		By("re-allocating into the freed slot")
		addrAgain, err := allocator.Allocate(ctx, client, ref, subnet, "pod-rel-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(addrAgain).To(Equal(addr), "re-allocation must claim the released offset 0")

		By("releasing the IP using allocator.Release without requestName (GET lookup path)")
		err = allocator.Release(ctx, client, ref, addrAgain)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns wrapped ErrSliceFull when an entire subnet is exhausted", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "full-test"}
		subnet := netip.MustParsePrefix("192.168.30.0/26") // exactly one /26 slice (64 IPs)
		trackSubnetSlices(ref, subnet)

		By("filling all 64 addresses in the slice")
		for i := 0; i < int(sliceAddresses); i++ {
			_, err := allocator.Allocate(ctx, client, ref, subnet, fmt.Sprintf("fill-%d", i))
			Expect(err).NotTo(HaveOccurred())
		}

		By("attempting a 65th allocation must return wrapped ErrSliceFull")
		_, err := allocator.Allocate(ctx, client, ref, subnet, "overflow-pod")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, allocator.ErrSliceFull)).To(BeTrue(),
			"error must wrap allocator.ErrSliceFull, got: %v", err)
	})

	It("rejects a subnet smaller than the fixed slice size", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "small-test"}
		subnet := netip.MustParsePrefix("192.168.40.0/27") // /27 is 32 addresses < 64

		_, err := allocator.Allocate(ctx, client, ref, subnet, "pod-0")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("smaller than the fixed slice size"))
	})
})
