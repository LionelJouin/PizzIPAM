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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
//
// It retries on an optimistic-concurrency Conflict. Every allocation for a block
// lives in one object (readme rule 3), so concurrent writers contend on that
// object's resourceVersion and etcd's compare-and-swap serializes them: a loser
// gets a 409 and retries -- it never corrupts state or double-allocates. Invalid
// errors (e.g. a full slice) are terminal and are NOT retried.
func applyRequest(ctx context.Context, name, requestName string) error {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: v4Prefix, PrefixLength: 26},
		Request:       []v1alpha1.Request{{Name: requestName}},
	}
	// TypeMeta is mandatory in an apply body (the server matches on apiVersion+kind);
	// the typed Create/Update path would otherwise fill it in for us.
	obj := &v1alpha1.IPSlice{
		TypeMeta:   metav1.TypeMeta{APIVersion: "multinetwork.networking.x-k8s.io/v1alpha1", Kind: "IPSlice"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	data, err := json.Marshal(obj) // JSON is valid YAML, so it is a valid apply body
	if err != nil {
		return err
	}

	force := true
	// A roomier backoff than retry.DefaultBackoff (5 steps): under a full 64-way
	// fill a loser may need several rounds before it wins its compare-and-swap.
	backoff := wait.Backoff{Steps: 20, Duration: 10 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Cap: 2 * time.Second}
	return retry.RetryOnConflict(backoff, func() error {
		// concurrentClient: this helper is only ever called from the fan-out
		// concurrency spec, which needs the raised rate limiter (see e2e_suite_test.go).
		_, err := concurrentClient.MultinetworkV1alpha1().IPSlices().Patch(
			ctx, name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: requestName, Force: &force},
		)
		return err
	})
}

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

	// ---- Anti-tamper -------------------------------------------------------
	// The status subresource is intentionally OFF, so a client *can* send a
	// status on create/update. The MutatingAdmissionPolicy defends this by
	// rebuilding status from oldObject on every write and ignoring the incoming
	// object's status entirely. These specs prove a client cannot forge or edit
	// an allocation. This is the core security property of the controllerless
	// design, so it is exercised directly rather than assumed.

	It("discards a client-forged status on create (anti-tamper)", func(ctx SpecContext) {
		By("creating a request-less slice that carries a bogus status.allocation")
		slice := newSlice() // no requests -> the allocator must produce an empty status
		slice.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "ghost", Offset: offsetLo, Address: v4Address(offsetLo)},
		}

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(BeEmpty(),
			"the allocator must overwrite a forged status, not trust the client's")
	})

	It("ignores a client-chosen offset for a real request on create (anti-tamper)", func(ctx SpecContext) {
		By("creating a one-request slice that pre-declares a 'wrong' offset for that request")
		slice := newSlice(v1alpha1.Request{Name: "alpha"}) // unconstrained -> allocator picks lowest free (offsetLo)
		forged := offsetHi                                 // anything other than the lowest free offset
		slice.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "alpha", Offset: forged, Address: v4Address(forged)},
		}

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.RequestName).To(Equal("alpha"))
		Expect(a.Offset).To(Equal(offsetLo), "the allocator, not the client, chooses the offset (lowest free)")
		Expect(a.Offset).NotTo(Equal(forged))
		Expect(a.Address).To(Equal(v4Address(a.Offset)))
	})

	It("reverts a client's attempt to edit an allocation on update (anti-tamper)", func(ctx SpecContext) {
		By("creating a one-request slice and recording its server-chosen allocation")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "alpha"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))
		orig := created.Status.Allocation[0]

		By("submitting an update that rewrites the allocation's offset and injects a ghost entry")
		tampered := orig.Offset + 1
		if tampered > offsetHi {
			tampered = offsetLo
		}
		Expect(tampered).NotTo(Equal(orig.Offset))
		created.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "alpha", Offset: tampered, Address: v4Address(tampered)},
			{RequestName: "ghost", Offset: offsetHi, Address: v4Address(offsetHi)},
		}
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(),
			"the allocator rebuilds status from oldObject, so the write is accepted with the corrected status")

		By("leaving the allocation exactly as the server first chose it")
		Expect(created.Status.Allocation).To(HaveLen(1), "the injected ghost entry must be dropped")
		Expect(created.Status.Allocation[0].RequestName).To(Equal("alpha"))
		Expect(created.Status.Allocation[0].Offset).To(Equal(orig.Offset),
			"an existing allocation's offset cannot be changed by the client")
		Expect(created.Status.Allocation[0].Address).To(Equal(orig.Address))
	})

	// ---- Validation rejections --------------------------------------------
	// Every invariant the CRD x-kubernetes-validations and the VAP enforce, in
	// one table. Each entry corrupts an otherwise-valid object so that exactly
	// the field under test is out of spec, and the write must be rejected at
	// admission (no controller ever sees an invalid object).

	DescribeTable("rejects an invalid IPSlice at admission (create)",
		func(ctx SpecContext, corrupt func(*v1alpha1.IPSlice)) {
			obj := newSlice(v1alpha1.Request{Name: "alpha"})
			corrupt(obj)
			var err error
			// Assign to `created` so AfterEach cleans up if a bug lets it through.
			created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, obj, metav1.CreateOptions{})
			Expect(err).To(HaveOccurred())
		},
		Entry("a non-canonical metadata.name", func(o *v1alpha1.IPSlice) {
			o.Name = "not-the-canonical-name"
		}),
		Entry("a slice whose prefixLength is not 26 for IPv4", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.PrefixLength = 25
			o.Name = naming.Name(o.Spec) // prefixLength is in the name now; keep it canonical so only the size rule fails
		}),
		Entry("a slice whose prefix is not the network address (not aligned)", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.Prefix = "192.168.0.1" // .1 is not the /26 network address
			o.Name = naming.Name(o.Spec)              // keep the name canonical so only the alignment rule fails
		}),
		Entry("a slice whose family does not match its prefix", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.Family = "IPv6" // prefix is still IPv4
			o.Spec.SliceSubnet.PrefixLength = 122
			o.Name = naming.Name(o.Spec)
		}),
		Entry("a request with offset but no length", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Offset = i32(4)
		}),
		Entry("a request with length but no offset", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Length = i32(4)
		}),
		Entry("a request whose offset is not aligned to its length", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Offset = i32(1) // 1 is not a multiple of 4
			o.Spec.Request[0].Length = i32(4)
		}),
		Entry("a request whose length is not a power of two", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Offset = i32(0)
			o.Spec.Request[0].Length = i32(3)
		}),
	)

	DescribeTable("rejects an invalid IPv6 IPSlice at admission (create)",
		func(ctx SpecContext, corrupt func(*v1alpha1.IPSlice)) {
			obj := newSliceV6(v1alpha1.Request{Name: "alpha"})
			corrupt(obj)
			var err error
			created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, obj, metav1.CreateOptions{})
			Expect(err).To(HaveOccurred())
		},
		Entry("a non-canonical IPv6 prefix", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.Prefix = "FE80::" // uppercase -> not canonical
			o.Name = naming.Name(o.Spec)         // naming canonicalizes, so isCanonical is the rule that fails
		}),
		Entry("an IPv6 prefix that is not aligned to /122", func(o *v1alpha1.IPSlice) {
			// fe80::20 has host bits set (offset 32 within the /122 block); the
			// /122 network of fe80::20 is fe80::, so the prefix is not aligned.
			// (fe80::40 would NOT work: 0x40 = offset 0 of the next /122, i.e. a
			// valid network address.)
			o.Spec.SliceSubnet.Prefix = "fe80::20"
			o.Name = naming.Name(o.Spec)
		}),
		Entry("an IPv6 slice whose prefixLength is not 122", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.PrefixLength = 64
			o.Name = naming.Name(o.Spec)
		}),
	)

	DescribeTable("rejects mutation of an immutable field (update)",
		func(ctx SpecContext, mutate func(*v1alpha1.IPSlice)) {
			By("creating a valid, constrained slice")
			var err error
			created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
				newSlice(v1alpha1.Request{Name: "alpha", Offset: i32(4), Length: i32(4)}),
				metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("mutating the immutable field and updating")
			mutate(created)
			_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred())
		},
		Entry("podNetworkRef", func(o *v1alpha1.IPSlice) {
			o.Spec.PodNetworkRef.Name = "changed"
		}),
		Entry("sliceSubnet", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.Prefix = "192.168.0.64" // a different, still-valid /26
		}),
		Entry("a request's offset", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Offset = i32(8) // still aligned to a length-4 window
		}),
		Entry("a request's length", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Length = i32(2) // offset 4 stays aligned to 2, so only immutability fails
		}),
	)
})
