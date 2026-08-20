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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	versioned "github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned"
	"k8s.io/client-go/tools/clientcmd"
)

// client is the generated IPSlice clientset, shared across specs. It is
// initialised once in BeforeSuite against the ambient kubeconfig.
var client versioned.Interface

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
})
