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
	"github.com/lioneljouin/pizzipam/pkg/naming"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every invariant the CRD x-kubernetes-validations and the VAP enforce, in
// one suite. Each test corrupts an otherwise-valid object so that exactly
// the field under test is out of spec, and the write must be rejected at
// admission (no controller ever sees an invalid object).
var _ = Describe("IPSlice validation", func() {
	var created *v1alpha1.IPSlice

	AfterEach(func(ctx SpecContext) {
		if created != nil {
			_ = client.MultinetworkV1alpha1().IPSlices().Delete(ctx, created.Name, metav1.DeleteOptions{})
			created = nil
		}
	})

	It("rejects adding more than one request in a single write", func(ctx SpecContext) {
		By("creating a slice with one request")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "a"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("attempting to add two requests (b and c) in the same write")
		created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: "b"}, v1alpha1.Request{Name: "c"})
		_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "adding multiple requests in one write must be rejected by CRD rule")
		Expect(err.Error()).To(ContainSubstring("every spec.request must be allocated in status"))
	})

	It("rejects removing more than one request in a single write", func(ctx SpecContext) {
		By("creating a slice with three requests")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx,
			newSlice(v1alpha1.Request{Name: "a"}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		for _, name := range []string{"b", "c"} {
			created.Spec.Request = append(created.Spec.Request, v1alpha1.Request{Name: name})
			created, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(created.Status.Allocation).To(HaveLen(3))

		By("attempting to remove two requests (b and c) in the same write")
		created.Spec.Request = []v1alpha1.Request{{Name: "a"}}
		_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "removing multiple requests in one write must be denied by VAP")
		Expect(err.Error()).To(ContainSubstring("at most one request may be removed in a single write"))
	})

	It("rejects changing or clearing spec.node while requests still exist", func(ctx SpecContext) {
		By("creating a slice with spec.node and one request")
		var err error
		obj := newSlice(v1alpha1.Request{Name: "a", Node: strPtr("worker-1")})
		obj.Spec.Node = strPtr("worker-1")
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, obj, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("attempting to clear spec.node while request 'a' is still present")
		created.Spec.Node = nil
		_, err = client.MultinetworkV1alpha1().IPSlices().Update(ctx, created, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "clearing spec.node while requests exist must be denied by VAP")
		Expect(err.Error()).To(ContainSubstring("spec.node cannot be changed while requests still exist in the slice"))
	})

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
		Entry("spec.node is set but request.node is missing", func(o *v1alpha1.IPSlice) {
			o.Spec.Node = strPtr("worker-1")
		}),
		Entry("spec.node does not match request.node", func(o *v1alpha1.IPSlice) {
			o.Spec.Node = strPtr("worker-1")
			o.Spec.Request[0].Node = strPtr("worker-2")
		}),
		Entry("request.name does not match deviceRef", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Name = "wrong-name"
			o.Spec.Request[0].DeviceRef = &v1alpha1.ResourceClaimDeviceRef{
				ClaimNamespace: "default",
				ClaimName:      "my-claim",
				Device:         "gpu-0",
			}
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
		Entry("an uncompressed IPv6 prefix with leading zeros", func(o *v1alpha1.IPSlice) {
			o.Spec.SliceSubnet.Prefix = "2001:0db8::" // leading zeros -> not canonical
			o.Name = naming.Name(o.Spec)              // naming canonicalizes to 2001:db8:: so only isCanonical fails
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
				newSlice(v1alpha1.Request{Name: "alpha", Offset: i32(4), Length: i32(4), Node: strPtr("worker-1")}),
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
		Entry("a request's node", func(o *v1alpha1.IPSlice) {
			o.Spec.Request[0].Node = strPtr("different-node")
		}),
	)
})
