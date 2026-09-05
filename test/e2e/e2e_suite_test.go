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

// Package e2e contains end-to-end tests that run against a real cluster with the
// CRD, MutatingAdmissionPolicy and ValidatingAdmissionPolicy from ./deployment
// already applied. It has no controller to wait for: allocation happens at
// admission time, so the create response already carries the allocated status.
//
// Run with a KUBECONFIG pointing at such a cluster:
//
//	kubectl apply -f ./deployment
//	go test ./test/e2e/... -v
package e2e

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	versioned "github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// client is the generated IPSlice clientset, shared across specs. It is
// initialised once in BeforeSuite against the ambient kubeconfig, with the
// default client-side rate limiter.
var client versioned.Interface

// concurrentClient is a second clientset with a raised rate limiter, used ONLY
// by the fan-out concurrency specs. The default limiter (QPS 5, Burst 10) would
// serialize their many simultaneous writes at the client's own token bucket and
// hide the server-side contention they mean to exercise. Every other spec makes
// single calls and uses the default `client`.
var concurrentClient versioned.Interface

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PizzIPAM e2e suite")
}

var _ = BeforeSuite(func() {
	// Load the config the same way kubectl does: KUBECONFIG, then ~/.kube/config,
	// then in-cluster. e2e requires a reachable cluster; fail loudly if there
	// isn't one rather than skipping silently.
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred(), "e2e needs a running cluster: could not load a kubeconfig")

	client, err = versioned.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	// Build the concurrency-only client from a copy of the same config with the
	// rate limiter raised. Mutating cfg here does not affect the already-built
	// `client`; NewForConfig snapshots what it needs at construction.
	cfg.QPS = 200
	cfg.Burst = 200
	concurrentClient, err = versioned.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	// A CRD is not necessarily served the instant it is Established: the
	// apiserver's discovery and RESTMapper can lag by a few seconds, and a write
	// in that window fails with a 503 ("... there can be a delay between when
	// CustomResourceDefinitions are created and when they are available"). This
	// bites a cold cluster (e.g. CI right after `kubectl apply`) but not a warm
	// one. Wait until a List of the custom resource actually succeeds so the
	// specs don't race the CRD becoming available.
	Eventually(func() error {
		_, err := client.MultinetworkV1alpha1().IPSlices().List(context.Background(), metav1.ListOptions{})
		return err
	}).WithTimeout(90*time.Second).WithPolling(time.Second).
		Should(Succeed(), "the IPSlice CRD never became servable")
})
