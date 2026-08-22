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

// Package naming defines the canonical object name for an IPSlice block.
//
// The name is a deterministic function of the block identity: podNetworkRef plus
// sliceSubnet.networkAddress. The slice size is a fixed /26 (see the CRD schema),
// so an aligned networkAddress identifies exactly one slice of the address space
// -- the size and the base pool are NOT part of the identity (including them would
// let two objects own overlapping ranges). This is the SINGLE SOURCE OF TRUTH
// clients use to address a block without a GET or LIST, and it is ENFORCED
// server-side: the ValidatingAdmissionPolicy in deployment/validating-policy.yaml
// recomputes the exact same string in CEL and rejects any IPSlice whose
// metadata.name differs.
//
// Because each identity maps to exactly one legal name, a second object for the
// same block collides (AlreadyExists) and a wrong name is denied -- so there can
// never be two objects for one block, and clients cannot invent their own names.
//
// IMPORTANT: if you change the format here, change the CEL expression in
// deployment/validating-policy.yaml in lockstep, or every create will be denied.
package naming

import (
	"fmt"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
)

// Name returns the canonical metadata.name for the given block identity.
//
// Format: "<name>.<kind>[.<namespace>].<sliceNet>"
// The string identity fields are DNS-1123 labels (enforced by the CRD schema) and
// sliceNet is a plain integer, so the result is a valid DNS-1123 subdomain and the
// mapping is unambiguous.
func Name(spec v1alpha1.IPSliceSpec) string {
	ns := ""
	if spec.PodNetworkRef.Namespace != nil && *spec.PodNetworkRef.Namespace != "" {
		ns = "." + *spec.PodNetworkRef.Namespace
	}

	return fmt.Sprintf("%s.%s%s.%d",
		spec.PodNetworkRef.Name,
		spec.PodNetworkRef.Kind,
		ns,
		spec.SliceSubnet.NetworkAddress,
	)
}
