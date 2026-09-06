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

var _ = Describe("IPSlice allocation", func() {
	var created *v1alpha1.IPSlice

	AfterEach(func(ctx SpecContext) {
		// Best-effort cleanup; the DRA driver owns lifecycle in production, but
		// tests must not leak cluster-scoped objects between specs.
		if created != nil {
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, created.Name, metav1.DeleteOptions{})
			created = nil
		}
	})

	It("allocates a free address at admission time", func(ctx SpecContext) {
		slice := newSlice(v1alpha1.Request{Name: "alpha"}) // no window -> anywhere in the slice

		By("creating the IPSlice")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The MutatingAdmissionPolicy fills status during admission, so the
		// create *response* already carries the allocation.
		By("observing status filled synchronously on the create response")
		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.RequestName).To(Equal("alpha"))
		Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", offsetLo), BeNumerically("<=", offsetHi)))
		Expect(a.Offset).To(Equal(offsetLo), "an unconstrained request takes the lowest free offset")
		Expect(a.Address).To(Equal(v4Address(a.Offset)))

		By("confirming a fresh Get returns the same allocation (persisted, not just echoed back)")
		fetched, err := client.MultinetworkV1alpha1().IPSlices().Get(ctx, created.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetched.Status.Allocation).To(ConsistOf(created.Status.Allocation))
	})

	It("allocates an IPv6 offset with no rendered address", func(ctx SpecContext) {
		By("creating an IPv6 slice with one request")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSliceV6(v1alpha1.Request{Name: "alpha"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "IPv6 slices must be accepted")

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.RequestName).To(Equal("alpha"))
		Expect(a.Offset).To(Equal(offsetLo), "the allocator picks the lowest free offset for IPv6 too")
		Expect(a.Address).To(BeEmpty(), "IPv6 stores the offset only; CEL cannot render a 128-bit address")
	})

	It("allocates an IPv6 offset on a non-zero aligned sub-slice (fe80::40/122)", func(ctx SpecContext) {
		By("creating an IPv6 slice for fe80::40/122")
		spec := v1alpha1.IPSliceSpec{
			PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
			SliceSubnet:   v1alpha1.Subnet{Family: "IPv6", Prefix: "fe80::40", PrefixLength: 122},
			Request:       []v1alpha1.Request{{Name: "r0"}},
		}
		name := naming.Name(spec)
		Expect(name).To(Equal("abc.blue-network.ipv6-fe80--40-122"))

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			&v1alpha1.IPSlice{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec},
			metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "fe80::40/122 must be accepted and aligned")

		Expect(created.Status.Allocation).To(HaveLen(1))
		Expect(created.Status.Allocation[0].Offset).To(Equal(int32(0)))
		Expect(created.Status.Allocation[0].Address).To(BeEmpty())

		By("adding a second request and verifying offset 1")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "r1"})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(2))
		Expect(created.Status.Allocation[1].Offset).To(Equal(int32(1)))
	})

	It("allocates an IPv6 offset within a window", func(ctx SpecContext) {
		By("creating an IPv6 slice with a constrained request [8, 15]")
		spec := v1alpha1.IPSliceSpec{
			PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
			SliceSubnet:   v1alpha1.Subnet{Family: "IPv6", Prefix: v6Prefix, PrefixLength: 122},
			Request: []v1alpha1.Request{
				{Name: "v6-win", Offset: i32(8), Length: i32(8)},
			},
		}
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			&v1alpha1.IPSlice{ObjectMeta: metav1.ObjectMeta{Name: naming.Name(spec)}, Spec: spec},
			metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.RequestName).To(Equal("v6-win"))
		Expect(a.Offset).To(Equal(int32(8)), "IPv6 window request must take lowest free offset in window")
		Expect(a.Address).To(BeEmpty())
	})

	It("handles namespaced PodNetworkRef correctly with canonical naming", func(ctx SpecContext) {
		ns := "prod-ns"
		spec := v1alpha1.IPSliceSpec{
			PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc", Namespace: &ns},
			SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.1.0", PrefixLength: 26},
			Request:       []v1alpha1.Request{{Name: "req-ns"}},
		}
		name := naming.Name(spec)
		Expect(name).To(Equal("abc.blue-network.prod-ns.ipv4-192-168-1-0-26"))

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			&v1alpha1.IPSlice{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec},
			metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "namespaced PodNetworkRef slice must be admitted")

		Expect(created.Status.Allocation).To(HaveLen(1))
		Expect(created.Status.Allocation[0].RequestName).To(Equal("req-ns"))
		Expect(created.Status.Allocation[0].Offset).To(Equal(int32(0)))
		Expect(created.Status.Allocation[0].Address).To(Equal("192.168.1.0"))
	})

	It("drives multi-slice allocation via pkg/allocator.Allocate against the live cluster", func(ctx SpecContext) {
		ref := &v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "live-walk"}
		subnet := netip.MustParsePrefix("192.168.2.0/25") // holds two /26 sub-slices: .0 and .64

		s0 := naming.Name(v1alpha1.IPSliceSpec{
			PodNetworkRef: *ref,
			SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.2.0", PrefixLength: 26},
		})
		s1 := naming.Name(v1alpha1.IPSliceSpec{
			PodNetworkRef: *ref,
			SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.2.64", PrefixLength: 26},
		})
		defer func() {
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, s0, metav1.DeleteOptions{})
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, s1, metav1.DeleteOptions{})
		}()

		By("allocating the first address from the subnet")
		addr1, err := allocator.Allocate(ctx, client, ref, subnet, "pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1).To(Equal(netip.MustParseAddr("192.168.2.0")))

		By("idempotently re-requesting the same name")
		addr1Again, err := allocator.Allocate(ctx, client, ref, subnet, "pod-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr1Again).To(Equal(addr1), "idempotent request must return the same address")

		By("allocating a second address")
		addr2, err := allocator.Allocate(ctx, client, ref, subnet, "pod-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(addr2).To(Equal(netip.MustParseAddr("192.168.2.1")))
	})

	It("accepts an IPSlice with no requests, then allocates and releases as they come and go", func(ctx SpecContext) {
		// Regression: the allocator must not choke when spec.request is absent.
		// Accessing object.spec.request directly in CEL throws "no such key" when
		// the field is omitted, so the allocator has to normalize it to an empty
		// list. This exercises that on both an empty create and a release-to-empty.
		By("creating an IPSlice with spec.request omitted entirely")
		slice := newSlice() // no requests -> spec.request is omitted
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "a request-less IPSlice must be accepted")
		Expect(created.Status.Allocation).To(BeEmpty())

		By("adding a request on a later write allocates an address")
		created.Spec.Request = []v1alpha1.Request{{Name: "alpha"}}
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))
		Expect(created.Status.Allocation[0].RequestName).To(Equal("alpha"))

		By("removing the last request releases the address and leaves spec.request absent")
		created.Spec.Request = nil
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "removing all requests must not choke on the absent spec.request")
		Expect(created.Status.Allocation).To(BeEmpty())
	})

	It("allocates from the request's window when one is given", func(ctx SpecContext) {
		// Ask for an offset inside the window [4, 7] (a length-4 window at offset 4).
		slice := newSlice(v1alpha1.Request{Name: "alpha", Offset: i32(4), Length: i32(4)})

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		By("keeping the offset inside the requested window")
		Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", int32(4)), BeNumerically("<=", int32(7))))
		Expect(a.Offset).To(Equal(int32(4)), "the lowest free offset in the window")
		Expect(a.Address).To(Equal(v4Address(a.Offset)))
	})

	It("allocates from a window that spans the whole slice", func(ctx SpecContext) {
		// A full-slice window (offset 0, length 64) is equivalent to no constraint.
		slice := newSlice(v1alpha1.Request{Name: "alpha", Offset: i32(0), Length: i32(64)})

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", offsetLo), BeNumerically("<=", offsetHi)))
	})

	It("rejects a request window that overflows the slice", func(ctx SpecContext) {
		// offset 32 + length 64 = 96 > slice size 64: the window does not fit.
		slice := newSlice(v1alpha1.Request{Name: "alpha", Offset: i32(32), Length: i32(64)})

		_, err := client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "a window that does not fit inside the slice must be rejected by admission")
	})

	It("serves requests one at a time and gives distinct offsets across writes", func(ctx SpecContext) {
		// Two requests for the SAME window must get different offsets, but only one
		// new request may be added per write (CEL cannot coordinate distinct picks
		// in one pass). So they are added across two writes.
		By("creating with a single constrained request")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "req0", Offset: i32(4), Length: i32(4)}),
			metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))

		By("adding a second request for the same window on a later write")
		created.Spec.Request = append(created.Spec.Request,
			v1alpha1.Request{Name: "req1", Offset: i32(4), Length: i32(4)})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(2))
		offsets := map[int32]string{}
		for _, a := range created.Status.Allocation {
			Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", int32(4)), BeNumerically("<=", int32(7))))
			offsets[a.Offset] = a.RequestName
		}
		Expect(offsets).To(HaveLen(2), "the two requests must get distinct offsets")
	})

	It("rejects a write that adds more than one new request at once", func(ctx SpecContext) {
		// Two brand-new requests in a single write: the allocator serves only the
		// first, the second stays unallocated, and the CRD rejects the write.
		slice := newSlice(
			v1alpha1.Request{Name: "one"},
			v1alpha1.Request{Name: "two"},
		)
		_, err := client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "adding two new requests in one write must be rejected")
	})

	It("fills every usable address, rejects when full, then re-accepts after a release", func(ctx SpecContext) {
		// A /26 has 64 allocatable offsets (0 - 63; network and broadcast included).
		// Add requests one per write (the allocator serves one new request per pass)
		// until the slice is full, a further request must be rejected, and then
		// removing a request must free its offset so the slice accepts again.
		usable := int(offsetHi - offsetLo + 1) // 64

		By("creating with the first request")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "req-0"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))

		By("adding the remaining requests one at a time until the slice is full")
		for i := 1; i < usable; i++ {
			created.Spec.Request = append(created.Spec.Request,
				v1alpha1.Request{Name: fmt.Sprintf("req-%d", i)})
			created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "request %d must be allocatable while the slice has room", i)
			Expect(created.Status.Allocation).To(HaveLen(i+1),
				"each write must allocate exactly one more address")
		}

		By("having allocated every usable offset exactly once")
		Expect(created.Status.Allocation).To(HaveLen(usable))
		seen := map[int32]bool{}
		for _, a := range created.Status.Allocation {
			Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", offsetLo), BeNumerically("<=", offsetHi)))
			Expect(a.Address).To(Equal(v4Address(a.Offset)))
			Expect(seen[a.Offset]).To(BeFalse(), "offset %d allocated twice", a.Offset)
			seen[a.Offset] = true
		}
		Expect(seen).To(HaveLen(usable), "every usable offset must be allocated exactly once")

		By("rejecting one more request because the slice is full")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "overflow"})
		_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "a request into a full slice must be rejected (no free address)")
		// The failed update did not change the stored object; `created` still holds
		// the last good, full state. Drop the un-appliable overflow request from it.
		created.Spec.Request = created.Spec.Request[:usable]

		By("releasing an address by removing a request")
		var freedOffset int32 = -1
		for _, a := range created.Status.Allocation {
			if a.RequestName == "req-0" {
				freedOffset = a.Offset
			}
		}
		Expect(freedOffset).NotTo(BeNumerically("<", 0), "req-0 should have an allocation to release")
		created.Spec.Request = created.Spec.Request[1:] // remove req-0
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(usable - 1))
		for _, a := range created.Status.Allocation {
			Expect(a.RequestName).NotTo(Equal("req-0"))
			Expect(a.Offset).NotTo(Equal(freedOffset), "the removed request's offset must be released, not still held")
		}

		By("re-accepting a new request into the freed slot")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "reuse"})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "a full slice must accept again once an address is freed")
		Expect(created.Status.Allocation).To(HaveLen(usable))
		var reuseOffset int32 = -1
		for _, a := range created.Status.Allocation {
			if a.RequestName == "reuse" {
				reuseOffset = a.Offset
			}
		}
		Expect(reuseOffset).To(Equal(freedOffset), "the freed offset should be handed to the next request (lowest free)")
	})

	It("fills the whole slice under concurrent applies and hands out every offset exactly once", func(ctx SpecContext) {
		// The concurrent analog of the sequential fill above. N clients each ask for
		// one IP on the SAME subnet at the same time. They all target the single
		// block object (readme rule 3), so etcd's compare-and-swap serializes the
		// writes: losers retry (inside applyRequest) rather than fail or
		// double-allocate. The invariant under test: all 64 usable offsets are handed
		// out exactly once, one per request, with no lost or duplicated allocation --
		// proving correctness under real contention, not just when the client
		// serializes the writes itself.
		usable := int(offsetHi - offsetLo + 1) // 64
		name := naming.Name(newSlice().Spec)
		// No single call owns the object here, so point `created` at it by name so
		// AfterEach still cleans it up.
		created = &v1alpha1.IPSlice{ObjectMeta: metav1.ObjectMeta{Name: name}}

		By(fmt.Sprintf("applying %d requests concurrently, one field manager per request", usable))
		var wg sync.WaitGroup
		errs := make([]error, usable)
		for i := 0; i < usable; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer GinkgoRecover()
				errs[i] = applyRequest(ctx, name, fmt.Sprintf("req-%d", i))
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			Expect(err).NotTo(HaveOccurred(),
				"concurrent apply of req-%d must succeed once conflicts are retried", i)
		}
		By("having allocated every usable offset exactly once")
		fetched, err := client.MultinetworkV1alpha1().IPSlices().Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetched.Status.Allocation).To(HaveLen(usable))
		seenOffset := map[int32]bool{}
		seenRequest := map[string]bool{}
		for _, a := range fetched.Status.Allocation {
			Expect(a.Offset).To(SatisfyAll(BeNumerically(">=", offsetLo), BeNumerically("<=", offsetHi)))
			Expect(a.Address).To(Equal(v4Address(a.Offset)))
			Expect(seenOffset[a.Offset]).To(BeFalse(), "offset %d handed to two requests under concurrency", a.Offset)
			seenOffset[a.Offset] = true
			seenRequest[a.RequestName] = true
		}
		Expect(seenOffset).To(HaveLen(usable), "every usable offset must be allocated exactly once")
		Expect(seenRequest).To(HaveLen(usable), "every request must get its own allocation")

		By("rejecting one more request into the now-full slice")
		Expect(applyRequest(ctx, name, "overflow")).To(HaveOccurred(),
			"a full slice must reject a further request even via the concurrent apply flow")
	})

	It("keeps existing allocations stable when another request is removed (no reshuffle)", func(ctx SpecContext) {
		// Removing one request must release only its offset; every other request must
		// keep the exact offset it already had. This is guaranteed by the allocator
		// carrying prior allocations over from oldObject, and backed by the CRD's
		// transition rule forbidding an existing allocation's offset from changing.
		By("building up four allocations, one request per write")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "a"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"b", "c", "d"} {
			created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: name})
			created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(created.Status.Allocation).To(HaveLen(4))

		before := map[string]int32{}
		for _, alloc := range created.Status.Allocation {
			before[alloc.RequestName] = alloc.Offset
		}

		By("removing a middle request (b)")
		kept := make([]v1alpha1.Request, 0, len(created.Spec.Request))
		for _, r := range created.Spec.Request {
			if r.Name != "b" {
				kept = append(kept, r)
			}
		}
		created.Spec.Request = kept
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("leaving every surviving allocation's offset exactly as it was")
		Expect(created.Status.Allocation).To(HaveLen(3))
		after := map[string]int32{}
		for _, alloc := range created.Status.Allocation {
			after[alloc.RequestName] = alloc.Offset
		}
		Expect(after).NotTo(HaveKey("b"), "the removed request's allocation must be gone")
		for _, name := range []string{"a", "c", "d"} {
			Expect(after).To(HaveKey(name))
			Expect(after[name]).To(Equal(before[name]),
				"request %q must keep its original offset after another request is removed", name)
		}
	})

	It("synchronizes status.bitmap accurately on creation, allocation and 1-by-1 release", func(ctx SpecContext) {
		By("creating a slice with one request: bit 0 must be 1, rest 0")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "a"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap).To(HaveLen(64))
		Expect(created.Status.Bitmap[0]).To(Equal(byte('1')))
		Expect(created.Status.Bitmap[1:]).To(Equal("000000000000000000000000000000000000000000000000000000000000000"))

		By("adding request b: bits 0 and 1 must be 1")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "b"})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap[:2]).To(Equal("11"))
		Expect(created.Status.Bitmap[2:]).To(Equal("00000000000000000000000000000000000000000000000000000000000000"))

		By("removing request a: bit 0 must become 0, bit 1 must remain 1")
		created.Spec.Request = []v1alpha1.Request{{Name: "b"}}
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap[:2]).To(Equal("01"))

		By("adding request c: offset 0 is free, so bit 0 must become 1 and offset 0 reused")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "c"})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap[:2]).To(Equal("11"))
		Expect(created.Status.Allocation).To(HaveLen(2))
		cAlloc := created.Status.Allocation[1]
		Expect(cAlloc.RequestName).To(Equal("c"))
		Expect(cAlloc.Offset).To(Equal(int32(0)), "freed offset 0 must be reused for request c")
	})
})
