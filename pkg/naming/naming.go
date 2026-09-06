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
// the sliceSubnet (family + canonical prefix + prefixLength). The slice size is
// fixed (see the CRD schema), so a network-aligned prefix identifies exactly one
// slice of the address space -- the size is NOT separately part of the identity
// (including it would let two objects own overlapping ranges). This is the SINGLE
// SOURCE OF TRUTH clients use to address a block without a GET or LIST, and it is
// ENFORCED server-side: the ValidatingAdmissionPolicy in
// deployment/validating-policy.yaml recomputes the exact same string in CEL and
// rejects any IPSlice whose metadata.name differs.
//
// Because each identity maps to exactly one legal name, a second object for the
// same block collides (AlreadyExists) and a wrong name is denied -- so there can
// never be two objects for one block, and clients cannot invent their own names.
// Uniqueness relies on the prefix being canonical: a text IP has many spellings,
// so Name canonicalizes it (net/netip) and the CRD enforces ip.isCanonical.
//
// IMPORTANT: if you change the format here, change the CEL expression in
// deployment/validating-policy.yaml in lockstep, or every create will be denied.
package naming

import (
	"fmt"
	"net/netip"
	"strings"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
)

// sliceSanitizer turns a canonical IP string into a DNS-1123-safe token: dots
// (IPv4) and colons (IPv6) both become hyphens. The mapping is injective on
// canonical strings within a family, and the family prefix separates families.
var sliceSanitizer = strings.NewReplacer(".", "-", ":", "-")

// Name returns the canonical metadata.name for the given block identity.
//
// Format: "<name>.<kind>[.<namespace>].<sliceId>" where
// sliceId = "<family-lower>-<sanitized-canonical-prefix>-<prefixLength>"
// (e.g. "ipv4-192-168-0-0-26" or "ipv6-fe80---122"). The string identity fields
// are DNS-1123 labels (enforced by the CRD schema) and the sanitized prefix keeps
// the result a valid DNS-1123 subdomain, so the mapping is unambiguous.
func Name(spec v1alpha1.IPSliceSpec) string {
	ns := ""
	if spec.PodNetworkRef.Namespace != nil && *spec.PodNetworkRef.Namespace != "" {
		ns = "." + *spec.PodNetworkRef.Namespace
	}

	// Canonicalize the prefix so the derived name matches the server's
	// ip.isCanonical-enforced form regardless of how the caller spelled it.
	prefix := spec.SliceSubnet.Prefix
	if addr, err := netip.ParseAddr(prefix); err == nil {
		prefix = addr.String()
	}

	sliceID := fmt.Sprintf("%s-%s-%d",
		strings.ToLower(spec.SliceSubnet.Family),
		sliceSanitizer.Replace(prefix),
		spec.SliceSubnet.PrefixLength,
	)

	return fmt.Sprintf("%s.%s%s.%s",
		spec.PodNetworkRef.Name,
		spec.PodNetworkRef.Kind,
		ns,
		sliceID,
	)
}

// RequestNameForDevice returns a deterministic request name for a DRA ResourceClaim device.
func RequestNameForDevice(ref v1alpha1.ResourceClaimDeviceRef) string {
	return fmt.Sprintf("%s.%s.%s", ref.ClaimNamespace, ref.ClaimName, ref.Device)
}
