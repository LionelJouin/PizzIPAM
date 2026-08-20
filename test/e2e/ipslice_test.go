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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// networkAddress is int(192.168.0.0); the slice is a /26 (192.168.0.0/26).
	networkAddress = int64(3232235520)
	sliceAddresses = int64(64) // /26 -> 62 usable (.1 - .62)
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

	It("allocates an IP for every request at admission time, with no controller", func(ctx SpecContext) {
		slice := &v1alpha1.IPSlice{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-ipslice-"},
			Spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
				BaseSubnet:    v1alpha1.Subnet{NetworkAddress: networkAddress, PrefixLength: 24, AddressSpace: 256},
				SliceSubnet:   v1alpha1.Subnet{NetworkAddress: networkAddress, PrefixLength: 26, AddressSpace: int32(sliceAddresses)},
				Request:       []v1alpha1.Request{{Name: "alpha"}, {Name: "beta"}},
			},
		}

		By("creating the IPSlice")
		var err error
		created, err = client.MultinetworkV1alpha1().IPSlices().Create(ctx, slice, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// The whole point of the design: no controller reconciles this later.
		// The MutatingAdmissionPolicy fills status during admission, so the
		// create *response* already carries the allocations.
		By("observing status filled synchronously on the create response")
		Expect(created.Status.Allocation).To(HaveLen(len(slice.Spec.Request)))

		lo := networkAddress + 1                  // skip the network address
		hi := networkAddress + sliceAddresses - 2 // skip the broadcast address
		seenIP := map[int64]bool{}
		seenReq := map[string]bool{}

		for _, a := range created.Status.Allocation {
			By(fmt.Sprintf("validating the allocation for request %q", a.RequestName))
			Expect(a.IP).To(SatisfyAll(BeNumerically(">=", lo), BeNumerically("<=", hi)),
				"allocated IP must be inside the slice's usable range")
			Expect(seenIP).NotTo(HaveKey(a.IP), "each allocated IP must be unique within the slice")
			Expect(a.Address).To(Equal(intToDottedIPv4(a.IP)),
				"status.address must be the dotted-decimal form of status.ip")
			seenIP[a.IP] = true
			seenReq[a.RequestName] = true
		}

		By("verifying every requested name received exactly one allocation")
		for _, r := range slice.Spec.Request {
			Expect(seenReq).To(HaveKey(r.Name))
		}

		By("confirming a fresh Get returns the same allocations (persisted, not just echoed back)")
		fetched, err := client.MultinetworkV1alpha1().IPSlices().Get(ctx, created.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetched.Status.Allocation).To(ConsistOf(created.Status.Allocation))
	})
})

// intToDottedIPv4 mirrors the allocator's ip -> address computation so the test
// can assert the two status fields stay consistent.
func intToDottedIPv4(ip int64) string {
	return fmt.Sprintf("%d.%d.%d.%d", ip/16777216%256, ip/65536%256, ip/256%256, ip%256)
}
