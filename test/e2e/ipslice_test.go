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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/naming"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// networkAddress is int(192.168.0.0); the slice is a /26 (192.168.0.0/26).
	networkAddress = int64(3232235520)
	sliceAddresses = int32(64) // /26 -> 62 usable (.1 - .62)

	lo = networkAddress + 1                         // first usable address
	hi = networkAddress + int64(sliceAddresses) - 2 // last usable address
)

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }

// dottedIPv4 mirrors the allocator's ip -> address computation so the test can
// assert the two status fields stay consistent.
func dottedIPv4(ip int64) string {
	return fmt.Sprintf("%d.%d.%d.%d", ip/16777216%256, ip/65536%256, ip/256%256, ip%256)
}

func newSlice(requests ...v1alpha1.Request) *v1alpha1.IPSlice {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{NetworkAddress: networkAddress, PrefixLength: 26, AddressSpace: sliceAddresses},
		Request:       requests,
	}
	// The name is server-enforced: it MUST be naming.Name(spec) or the
	// ValidatingAdmissionPolicy denies the write. GenerateName would fail.
	return &v1alpha1.IPSlice{
		ObjectMeta: metav1.ObjectMeta{Name: naming.Name(spec)},
		Spec:       spec,
	}
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

	It("allocates a free IP at admission time", func(ctx SpecContext) {
		slice := newSlice(v1alpha1.Request{Name: "alpha"}) // no subnet -> anywhere in the slice

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
		Expect(a.IP).To(SatisfyAll(BeNumerically(">=", lo), BeNumerically("<=", hi)))
		Expect(a.Address).To(Equal(dottedIPv4(a.IP)))

		By("confirming a fresh Get returns the same allocation (persisted, not just echoed back)")
		fetched, err := client.MultinetworkV1alpha1().IPSlices().Get(ctx, created.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetched.Status.Allocation).To(ConsistOf(created.Status.Allocation))
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

		By("adding a request on a later write allocates an IP")
		created.Spec.Request = []v1alpha1.Request{{Name: "alpha"}}
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))
		Expect(created.Status.Allocation[0].RequestName).To(Equal("alpha"))

		By("removing the last request releases the IP and leaves spec.request absent")
		created.Spec.Request = nil
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "removing all requests must not choke on the absent spec.request")
		Expect(created.Status.Allocation).To(BeEmpty())
	})

	It("allocates from the request's subnet when one is given", func(ctx SpecContext) {
		// Ask for an IP inside 192.168.0.4/30 (.4 - .7).
		slice := newSlice(v1alpha1.Request{Name: "alpha", NetworkAddress: i64(networkAddress + 4), PrefixLength: i32(30)})

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		By("keeping the IP inside the requested subnet")
		Expect(a.IP).To(SatisfyAll(BeNumerically(">=", networkAddress+4), BeNumerically("<=", networkAddress+7)))
		Expect(a.Address).To(Equal(dottedIPv4(a.IP)))
	})

	It("allocates from a subnet that contains the slice", func(ctx SpecContext) {
		// A broader subnet (192.168.0.0/24) contains the slice -> allocate from the slice.
		slice := newSlice(v1alpha1.Request{Name: "alpha", NetworkAddress: i64(networkAddress), PrefixLength: i32(24)})

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.IP).To(SatisfyAll(BeNumerically(">=", lo), BeNumerically("<=", hi)))
	})

	It("rejects a request whose subnet is disjoint from the slice", func(ctx SpecContext) {
		// 192.168.0.64/26 is a different /26, disjoint from this slice.
		slice := newSlice(v1alpha1.Request{Name: "alpha", NetworkAddress: i64(networkAddress + 64), PrefixLength: i32(26)})

		_, err := client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "a disjoint subnet must be rejected by admission")
	})

	It("serves requests one at a time and gives distinct IPs across writes", func(ctx SpecContext) {
		// Two requests for the SAME /30 must get different IPs, but only one new
		// request may be added per write (CEL cannot coordinate distinct picks in
		// one pass). So they are added across two writes.
		By("creating with a single constrained request")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "req0", NetworkAddress: i64(networkAddress + 4), PrefixLength: i32(30)}),
			metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))

		By("adding a second request for the same subnet on a later write")
		created.Spec.Request = append(created.Spec.Request,
			v1alpha1.Request{Name: "req1", NetworkAddress: i64(networkAddress + 4), PrefixLength: i32(30)})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(2))
		ips := map[int64]string{}
		for _, a := range created.Status.Allocation {
			Expect(a.IP).To(SatisfyAll(BeNumerically(">=", networkAddress+4), BeNumerically("<=", networkAddress+7)))
			ips[a.IP] = a.RequestName
		}
		Expect(ips).To(HaveLen(2), "the two requests must get distinct IPs")
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
		// A /26 has 62 usable addresses (.1 - .62). Add requests one per write
		// (the allocator serves one new request per pass) until the slice is full,
		// a further request must be rejected, and then removing a request must free
		// its IP so the slice accepts a new request again (full is not permanent).
		usable := int(hi - lo + 1) // 62

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

		By("having allocated every usable address exactly once")
		Expect(created.Status.Allocation).To(HaveLen(usable))
		seen := map[int64]bool{}
		for _, a := range created.Status.Allocation {
			Expect(a.IP).To(SatisfyAll(BeNumerically(">=", lo), BeNumerically("<=", hi)))
			Expect(a.Address).To(Equal(dottedIPv4(a.IP)))
			Expect(seen[a.IP]).To(BeFalse(), "IP %d allocated twice", a.IP)
			seen[a.IP] = true
		}
		Expect(seen).To(HaveLen(usable), "every usable address must be allocated exactly once")

		By("rejecting one more request because the slice is full")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "overflow"})
		_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "a request into a full slice must be rejected (no free address)")
		// The failed update did not change the stored object; `created` still holds
		// the last good, full state. Drop the un-appliable overflow request from it.
		created.Spec.Request = created.Spec.Request[:usable]

		By("releasing an address by removing a request")
		var freedIP int64
		for _, a := range created.Status.Allocation {
			if a.RequestName == "req-0" {
				freedIP = a.IP
			}
		}
		Expect(freedIP).NotTo(BeZero(), "req-0 should have an allocation to release")
		created.Spec.Request = created.Spec.Request[1:] // remove req-0
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(usable - 1))
		for _, a := range created.Status.Allocation {
			Expect(a.RequestName).NotTo(Equal("req-0"))
			Expect(a.IP).NotTo(Equal(freedIP), "the removed request's IP must be released, not still held")
		}

		By("re-accepting a new request into the freed slot")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "reuse"})
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "a full slice must accept again once an address is freed")
		Expect(created.Status.Allocation).To(HaveLen(usable))
		var reuseIP int64
		for _, a := range created.Status.Allocation {
			if a.RequestName == "reuse" {
				reuseIP = a.IP
			}
		}
		Expect(reuseIP).To(Equal(freedIP), "the freed address should be handed to the next request (lowest free)")
	})

	It("keeps existing allocations stable when another request is removed (no reshuffle)", func(ctx SpecContext) {
		// Removing one request must release only its IP; every other request must
		// keep the exact address it already had. This is guaranteed by the allocator
		// carrying prior allocations over from oldObject, and backed by the CRD's
		// transition rule forbidding an existing allocation's IP from changing.
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

		before := map[string]int64{}
		for _, alloc := range created.Status.Allocation {
			before[alloc.RequestName] = alloc.IP
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

		By("leaving every surviving allocation's IP exactly as it was")
		Expect(created.Status.Allocation).To(HaveLen(3))
		after := map[string]int64{}
		for _, alloc := range created.Status.Allocation {
			after[alloc.RequestName] = alloc.IP
		}
		Expect(after).NotTo(HaveKey("b"), "the removed request's allocation must be gone")
		for _, name := range []string{"a", "c", "d"} {
			Expect(after).To(HaveKey(name))
			Expect(after[name]).To(Equal(before[name]),
				"request %q must keep its original IP after another request is removed", name)
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
			{RequestName: "ghost", IP: lo, Address: dottedIPv4(lo)},
		}

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(BeEmpty(),
			"the allocator must overwrite a forged status, not trust the client's")
	})

	It("ignores a client-chosen IP for a real request on create (anti-tamper)", func(ctx SpecContext) {
		By("creating a one-request slice that pre-declares a 'wrong' IP for that request")
		slice := newSlice(v1alpha1.Request{Name: "alpha"}) // unconstrained -> allocator picks lowest free (lo)
		forged := hi                                       // anything other than the lowest free address
		slice.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "alpha", IP: forged, Address: dottedIPv4(forged)},
		}

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(created.Status.Allocation).To(HaveLen(1))
		a := created.Status.Allocation[0]
		Expect(a.RequestName).To(Equal("alpha"))
		Expect(a.IP).To(Equal(lo), "the allocator, not the client, chooses the IP (lowest free)")
		Expect(a.IP).NotTo(Equal(forged))
		Expect(a.Address).To(Equal(dottedIPv4(a.IP)))
	})

	It("reverts a client's attempt to edit an allocation on update (anti-tamper)", func(ctx SpecContext) {
		By("creating a one-request slice and recording its server-chosen allocation")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "alpha"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Allocation).To(HaveLen(1))
		orig := created.Status.Allocation[0]

		By("submitting an update that rewrites the allocation's IP and injects a ghost entry")
		tampered := orig.IP + 1
		if tampered > hi {
			tampered = lo
		}
		Expect(tampered).NotTo(Equal(orig.IP))
		created.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "alpha", IP: tampered, Address: dottedIPv4(tampered)},
			{RequestName: "ghost", IP: hi, Address: dottedIPv4(hi)},
		}
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(),
			"the allocator rebuilds status from oldObject, so the write is accepted with the corrected status")

		By("leaving the allocation exactly as the server first chose it")
		Expect(created.Status.Allocation).To(HaveLen(1), "the injected ghost entry must be dropped")
		Expect(created.Status.Allocation[0].RequestName).To(Equal("alpha"))
		Expect(created.Status.Allocation[0].IP).To(Equal(orig.IP),
			"an existing allocation's IP cannot be changed by the client")
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
		Entry("a slice that is not a /26 (prefixLength != 26)", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.PrefixLength = 25 // name is unaffected (naming.Name ignores prefixLength)
		}),
		Entry("a slice whose addressSpace is not 64", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.AddressSpace = 32
		}),
		Entry("a slice networkAddress not aligned to the /26 grid", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.NetworkAddress = networkAddress + 1
			o.Name = naming.Name(o.Spec) // keep the name canonical so only the %64 rule fails
		}),
		Entry("a request with networkAddress but no prefixLength", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].NetworkAddress = i64(networkAddress)
		}),
		Entry("a request with prefixLength but no networkAddress", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].PrefixLength = i32(30)
		}),
		Entry("a request subnet not aligned to its own prefixLength", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].NetworkAddress = i64(networkAddress + 1) // .1 is not aligned to a /30
			o.Spec.Request[0].PrefixLength = i32(30)
		}),
	)

	DescribeTable("rejects mutation of an immutable field (update)",
		func(ctx SpecContext, mutate func(*v1alpha1.IPSlice)) {
			By("creating a valid, constrained slice")
			var err error
			created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
				newSlice(v1alpha1.Request{Name: "alpha", NetworkAddress: i64(networkAddress + 4), PrefixLength: i32(30)}),
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
			o.Spec.SliceSubnet.NetworkAddress = networkAddress + 64 // a different, still-valid /26
		}),
		Entry("a request's networkAddress", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].NetworkAddress = i64(networkAddress + 8) // still a valid /30 network
		}),
		Entry("a request's prefixLength", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].PrefixLength = i32(31) // networkAddress+4 stays aligned to /31, so only immutability fails
		}),
	)
})
