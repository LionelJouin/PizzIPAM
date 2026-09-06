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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The status subresource is intentionally OFF, so a client *can* send a
// status on create/update. The MutatingAdmissionPolicy defends this by
// rebuilding status from oldObject on every write and ignoring the incoming
// object's status entirely. These specs prove a client cannot forge or edit
// an allocation. This is the core security property of the controllerless
// design, so it is exercised directly rather than assumed.
var _ = Describe("IPSlice anti-tamper", func() {
	var created *v1alpha1.IPSlice

	AfterEach(func(ctx SpecContext) {
		if created != nil {
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, created.Name, metav1.DeleteOptions{})
			created = nil
		}
	})

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

	It("discards a client-forged status.bitmap on create and update (anti-tamper)", func(ctx SpecContext) {
		By("creating a one-request slice that pre-declares an inverted bitmap")
		slice := newSlice(v1alpha1.Request{Name: "alpha"})
		slice.Status.Bitmap = "0111111111111111111111111111111111111111111111111111111111111111"

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap[0]).To(Equal(byte('1')), "the allocator must overwrite client-forged bitmap")
		Expect(created.Status.Bitmap[1:]).To(Equal("000000000000000000000000000000000000000000000000000000000000000"))

		By("updating the slice with a tampered bitmap")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "beta"})
		created.Status.Bitmap = "0000000000000000000000000000000000000000000000000000000000000000"
		created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Status.Bitmap[:2]).To(Equal("11"), "the allocator must preserve real allocations in bitmap")
	})

	It("discards a client-supplied IPv6 address in status on create (anti-tamper)", func(ctx SpecContext) {
		By("creating an IPv6 slice that carries a client-supplied address in status")
		slice := newSliceV6(v1alpha1.Request{Name: "alpha"})
		slice.Status.Allocation = []v1alpha1.Allocation{
			{RequestName: "alpha", Offset: 0, Address: "fe80::1"},
		}

		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "MAP overwrites status, so the write succeeds with corrected status")
		Expect(created.Status.Allocation).To(HaveLen(1))
		Expect(created.Status.Allocation[0].Address).To(BeEmpty(), "the allocator must omit address for IPv6")
	})
})
