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

// Package allocator is the client-side helper for requesting an IP address from
// an IPSlice. It hides the allocation protocol described in docs/readme.md so a
// consumer only has to say WHICH block it wants (a pod-network reference plus a
// slice subnet) and under WHICH request name, and gets back a concrete IP.
//
// The flow is a single self-describing round-trip (readme rule 1): the object
// name is a deterministic function of the block identity (pkg/naming), so the
// helper computes it locally and issues a server-side apply carrying only this
// caller's own request entry. Because spec.request is a map-list keyed by name,
// independent callers each own their own entry and never clobber each other, so
// no prior GET or LIST is needed. The apiserver serializes concurrent writers on
// the single block object, so a lost optimistic-concurrency race is retried
// transparently here (readme rule 3).
//
// The returned address is derived from (prefix, offset), which works for every
// family: IPv4 slices also carry a rendered address in status, but IPv6 slices
// carry the offset only (CEL cannot render a 128-bit address), so computing it
// client-side is what keeps this helper family-agnostic (readme rule 7).
package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	versioned "github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned"
	"github.com/lioneljouin/pizzipam/pkg/naming"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// ErrSliceFull is returned (wrapped) when the slice has no free address left for
// the request. Callers walking a larger subnet can test for it with errors.Is
// and move on to the next slice.
var ErrSliceFull = errors.New("ip slice is full")

// A full slice surfaces as one of two distinct Invalid admission rejections,
// and Allocate must recognise both so it can report ErrSliceFull rather than a
// generic error (and so a caller walking a larger subnet knows to move on):
//
//   - causeTooMany: adding one more request entry exceeds spec.request's MaxItems
//     cap (64). This fires first when a slice is filled with distinct request
//     names, and it makes the apiserver SKIP the CEL rules, so the marker below
//     is absent in this case.
//   - sliceFullMarker: the stable part of the CRD's "every spec.request must be
//     allocated in status" rule (apis/v1alpha1/types.go), which fires when the
//     offsets are exhausted while the request list is still within its cap
//     (e.g. windowed requests competing for the same offsets).
const (
	sliceFullMarker = "must be allocated in status"
	causeTooMany    = metav1.CauseType("FieldValueTooMany") // field.ErrorTypeTooMany, i.e. a MaxItems violation
)

// isSliceFull reports whether err is an admission rejection meaning the slice
// has no room for the request, as opposed to a malformed request. It inspects
// the structured causes and falls back to the raw message text.
func isSliceFull(err error) bool {
	if !apierrors.IsInvalid(err) {
		return false
	}
	var se *apierrors.StatusError
	if errors.As(err, &se) && se.ErrStatus.Details != nil {
		for _, c := range se.ErrStatus.Details.Causes {
			if c.Type == causeTooMany || strings.Contains(c.Message, sliceFullMarker) {
				return true
			}
		}
	}
	return strings.Contains(err.Error(), sliceFullMarker) ||
		strings.Contains(err.Error(), "must have at most")
}

// retryBackoff bounds the client-side retry of a transient apply failure (see
// isRetryable). Small, jittered, and deep so a contended write resolves without
// a lockstep herd. It is not the governor of how long Allocate keeps trying --
// the caller's context deadline is (see isRetryable) -- but it caps the attempts
// and spaces them so retries don't all re-collide at once.
var retryBackoff = wait.Backoff{Steps: 60, Duration: 25 * time.Millisecond, Factor: 1.3, Jitter: 1.0, Cap: 1500 * time.Millisecond}

// isRetryable reports whether a failed apply is worth retrying with the same
// (idempotent) request. It covers the two ways a write to a hotly-contended
// slice object fails:
//
//   - Conflict (409): a lost optimistic-concurrency race. This rarely reaches
//     the client -- our applies carry no resourceVersion precondition, so the
//     apiserver's storage layer (GuaranteedUpdate) resolves most conflicts
//     INTERNALLY by re-reading and re-running admission in a loop -- but a
//     field-manager conflict or a caller that pins a resourceVersion can surface
//     one.
//   - Timeout (504, "request did not complete within requested timeout") /
//     ServerTimeout / TooManyRequests: the apiserver gave up (its internal
//     conflict loop ran past the request deadline) or is shedding load. This IS
//     the error a concurrent fill of a small pool produces. A timeout is
//     ambiguous -- the write may or may not have landed -- but because the apply
//     is a server-side apply keyed by requestName, re-applying the SAME request
//     is idempotent (readme rule 8): the retry either finds our allocation
//     already present or makes it, and never double-allocates.
//
// Retrying does NOT lower the O(slice_size^2) server cost of a concurrent fill
// (see docs/readme.md); it converts the spurious deadline failures back into
// (slow) successes, provided the pool actually has room. The caller's context is
// the real governor: once its deadline passes, the next apply returns a bare
// context error -- which is none of the above, so the retry loop stops. Callers
// should therefore pass a bounded context.
func isRetryable(err error) bool {
	return apierrors.IsConflict(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err)
}

// sliceHosts is the fixed number of addresses in every slice -- a /26 for IPv4,
// a /122 for IPv6 (apis/v1alpha1/types.go). It is therefore also the step, in
// addresses, between two successive slice network addresses.
const sliceHosts = 64

// sliceBitsFor returns the fixed slice prefix length for prefix's family: 26 for
// IPv4, 122 for IPv6 (both cover sliceHosts addresses).
func sliceBitsFor(prefix netip.Addr) int {
	if prefix.Is4() {
		return 26
	}
	return 122
}

// Allocate requests a single IP address from subnet for the given request,
// and returns the allocated address.
//
// A request must be specified via WithRequest, WithName, or WithDeviceRef.
//
// The slice size is a fixed system-wide constant (a /26 for IPv4, a /122 for
// IPv6). When subnet is exactly one slice, Allocate operates on it directly.
// When subnet is larger, Allocate walks its slice-sized sub-blocks and allocates
// from the FIRST one it visits with room (readme's "walking a larger subnet").
// Pass WithNode to optimize the walk for node affinity.
//
// It is idempotent per request name WITHIN a single slice: calling it again with
// the same arguments returns the same address (the request entry is already
// present, so the server keeps its existing offset -- readme rule 8). Across a
// multi-slice subnet the walk is first-fit and does not search other slices for
// an existing entry, so callers that rely on idempotency should pass a single
// slice.
//
// Errors:
//   - ErrSliceFull (wrapped) if no slice in subnet has a free address;
//   - a wrapped admission error for any other rejection (e.g. a malformed
//     subnet or an out-of-spec request);
//   - the underlying client error for transport/permission failures.
func Allocate(
	ctx context.Context,
	client versioned.Interface,
	networkRef *v1alpha1.PodNetworkRef,
	subnet netip.Prefix,
	opts ...Option,
) (netip.Addr, error) {
	opt := options{retry: true}
	for _, o := range opts {
		o(&opt)
	}

	if networkRef == nil {
		return netip.Addr{}, errors.New("networkRef must not be nil")
	}
	if opt.request == nil {
		return netip.Addr{}, errors.New("request is required (use WithRequest, WithName, or WithDeviceRef)")
	}
	if opt.request.RequestName() == "" {
		return netip.Addr{}, errors.New("request name must not be empty")
	}
	if opt.nodeLock != "" {
		if opt.request.Node() == "" {
			return netip.Addr{}, fmt.Errorf("WithNode(%q) requires request node to be set", opt.nodeLock)
		}
		if opt.request.Node() != opt.nodeLock {
			return netip.Addr{}, fmt.Errorf("request node %q does not match WithNode(%q)", opt.request.Node(), opt.nodeLock)
		}
	}
	if !subnet.IsValid() {
		return netip.Addr{}, fmt.Errorf("invalid subnet %q", subnet)
	}

	// Unmap so an IPv4 subnet reports Is4 and renders in dotted form.
	base := subnet.Masked().Addr().Unmap()
	sliceBits := sliceBitsFor(base)
	if subnet.Bits() > sliceBits {
		return netip.Addr{}, fmt.Errorf(
			"subnet /%d is smaller than the fixed slice size /%d", subnet.Bits(), sliceBits)
	}

	// Number of slice-sized sub-blocks in subnet is 2^(sliceBits-subnetBits). The
	// guard keeps the shift (and the walk) sane: anything past this is an
	// impractical number of API round-trips and would overflow the counter.
	shift := sliceBits - subnet.Bits()
	if shift > 32 {
		return netip.Addr{}, fmt.Errorf(
			"subnet /%d is too large to walk as /%d slices", subnet.Bits(), sliceBits)
	}
	numSlices := uint64(1) << uint(shift) //nolint:gosec // shift <= 32

	fullSlices := make(map[uint64]bool)
	outerBackoff := wait.Backoff{
		Steps:    100,
		Duration: 10 * time.Millisecond,
		Factor:   1.3,
		Jitter:   1.0,
		Cap:      100 * time.Millisecond,
	}

	var sliceBackoff wait.Backoff
	if !opt.retry {
		sliceBackoff = wait.Backoff{Steps: 1}
	} else if numSlices == 1 {
		sliceBackoff = retryBackoff
	} else {
		// Multi-slice: try 2 times locally on this slice before hopping.
		sliceBackoff = wait.Backoff{Steps: 2, Duration: 5 * time.Millisecond, Factor: 1.5, Jitter: 1.0, Cap: 15 * time.Millisecond}
	}

	for {
		var lastRetryableErr error
		allRemainingFull := true

		for _, i := range opt.visitOrder(numSlices) {
			if fullSlices[i] {
				continue
			}
			allRemainingFull = false

			slice := netip.PrefixFrom(addAddr(base, i*sliceHosts), sliceBits)
			got, err := allocateSlice(ctx, client, networkRef, slice, opt.request, opt.nodeLock, sliceBackoff)
			if err == nil {
				return got, nil
			}
			if errors.Is(err, ErrSliceFull) {
				fullSlices[i] = true
				continue // this slice is full; try the next one
			}
			if isNodeMismatch(err) {
				// Slice is claimed by another node; hop to probe next slice.
				continue
			}
			if isRetryable(err) {
				lastRetryableErr = err
				// In a multi-slice subnet, hop to another slice rather than dogpiling.
				continue
			}
			return netip.Addr{}, err
		}

		if allRemainingFull || len(fullSlices) == int(numSlices) {
			return netip.Addr{}, fmt.Errorf("%w: no free address in subnet %s", ErrSliceFull, subnet)
		}

		if !opt.retry {
			if lastRetryableErr != nil {
				return netip.Addr{}, lastRetryableErr
			}
			return netip.Addr{}, fmt.Errorf("%w: no free address in subnet %s", ErrSliceFull, subnet)
		}

		select {
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		case <-time.After(outerBackoff.Step()):
		}
	}
}

// Release frees an allocated IP address from its IPSlice.
//
// The slice holding the address is determined deterministically from (networkRef, ip).
// When a request is provided (via WithRequest, WithName, or WithDeviceRef), Release issues
// a single server-side apply without a prior GET to remove the request under its field manager.
// If no request is given, Release performs a single GET to look up which request owns the
// IP's offset before releasing it.
func Release(
	ctx context.Context,
	client versioned.Interface,
	networkRef *v1alpha1.PodNetworkRef,
	ip netip.Addr,
	opts ...Option,
) error {
	opt := options{retry: true}
	for _, o := range opts {
		o(&opt)
	}

	if networkRef == nil {
		return errors.New("networkRef must not be nil")
	}
	if !ip.IsValid() {
		return errors.New("ip must be a valid address")
	}

	base := ip.Unmap()
	family := "IPv4"
	sliceBits := 26
	if !base.Is4() {
		family = "IPv6"
		sliceBits = 122
	}

	slice := netip.PrefixFrom(base, sliceBits).Masked()
	offset := offsetForAddr(slice.Addr(), base)

	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: *networkRef,
		SliceSubnet: v1alpha1.Subnet{
			Family:       family,
			Prefix:       slice.Addr().String(),
			PrefixLength: int32(sliceBits),
		},
	}
	name := naming.Name(spec)

	var reqName string
	if opt.request != nil {
		reqName = opt.request.RequestName()
	}
	if reqName == "" {
		sliceObj, err := client.MultinetworkV1alpha1().IPSlices().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil // slice does not exist -> already released
			}
			return fmt.Errorf("get IPSlice %q to resolve request for IP %s: %w", name, ip, err)
		}
		for _, a := range sliceObj.Status.Allocation {
			if a.Offset == offset {
				reqName = a.RequestName
				break
			}
		}
		if reqName == "" {
			return nil // IP offset is not currently allocated -> nothing to release
		}
	}

	spec.Request = nil
	obj := &v1alpha1.IPSlice{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "IPSlice"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal apply body: %w", err)
	}

	force := true
	apply := func() error {
		_, applyErr := client.MultinetworkV1alpha1().IPSlices().Patch(
			ctx, name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: reqName, Force: &force},
		)
		return applyErr
	}
	if opt.retry {
		err = retry.OnError(retryBackoff, isRetryable, apply)
	} else {
		err = apply()
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release IP %s on IPSlice %q: %w", ip, name, err)
	}
	return nil
}

// offsetForAddr returns the host offset of addr within a slice network address prefix.
func offsetForAddr(prefix, addr netip.Addr) int32 {
	if addr.Is4() {
		return int32(addr.As4()[3] - prefix.As4()[3])
	}
	return int32(addr.As16()[15] - prefix.As16()[15])
}

// isNodeMismatch reports whether err is an admission rejection indicating the slice
// is currently owned by a different node.
func isNodeMismatch(err error) bool {
	return apierrors.IsInvalid(err) &&
		(strings.Contains(err.Error(), "matching spec.node") ||
			strings.Contains(err.Error(), "spec.node cannot be changed"))
}

// allocateSlice performs the single self-describing round-trip against one
// slice-sized block (slice must already be masked to its network address). It is
// the per-slice unit that Allocate's subnet walk drives.
func allocateSlice(
	ctx context.Context,
	client versioned.Interface,
	networkRef *v1alpha1.PodNetworkRef,
	slice netip.Prefix,
	reqSpec Request,
	nodeLock string,
	backoff wait.Backoff,
) (netip.Addr, error) {
	prefix := slice.Addr()
	family := "IPv4"
	if !prefix.Is4() {
		family = "IPv6"
	}

	var req v1alpha1.Request
	reqSpec.ApplyToRequest(&req)

	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: *networkRef,
		SliceSubnet: v1alpha1.Subnet{
			Family:       family,
			Prefix:       prefix.String(),
			PrefixLength: int32(slice.Bits()),
		},
		Request: []v1alpha1.Request{req},
	}
	if nodeLock != "" {
		spec.Node = &nodeLock
	}
	name := naming.Name(spec)

	// TypeMeta is mandatory in an apply body (the server matches on apiVersion+kind).
	obj := &v1alpha1.IPSlice{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "IPSlice"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	data, err := json.Marshal(obj) // JSON is valid YAML, so it is a valid apply body
	if err != nil {
		return netip.Addr{}, fmt.Errorf("marshal apply body: %w", err)
	}

	force := true
	var result *v1alpha1.IPSlice
	apply := func() error {
		var applyErr error
		result, applyErr = client.MultinetworkV1alpha1().IPSlices().Patch(
			ctx, name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: req.Name, Force: &force},
		)
		return applyErr
	}
	if backoff.Steps > 1 {
		err = retry.OnError(backoff, isRetryable, apply)
	} else {
		err = apply()
	}
	if err != nil {
		if isSliceFull(err) {
			return netip.Addr{}, fmt.Errorf("%w: %v", ErrSliceFull, err)
		}
		return netip.Addr{}, fmt.Errorf("apply IPSlice %q: %w", name, err)
	}

	// Allocation is filled synchronously at admission, so the apply response
	// already carries our offset.
	for _, a := range result.Status.Allocation {
		if a.RequestName == req.Name {
			return addrForOffset(prefix, a.Offset), nil
		}
	}
	return netip.Addr{}, fmt.Errorf(
		"IPSlice %q applied but returned no allocation for request %q", name, req.Name)
}

// addrForOffset returns prefix + offset, the concrete address of a slice host.
func addrForOffset(prefix netip.Addr, offset int32) netip.Addr {
	return addAddr(prefix, uint64(offset)) //nolint:gosec // offset is a slice host offset (0..63)
}

// addAddr returns addr + delta. It adds delta as a big-endian integer across the
// full 16-byte form, so it is correct for both IPv4 and IPv6 regardless of where
// the address sits; the result is unmapped back to IPv4 when addr was IPv4. It is
// used both to place a host within a slice (delta = offset) and to step from one
// slice network address to the next (delta = a multiple of sliceHosts).
func addAddr(addr netip.Addr, delta uint64) netip.Addr {
	b := addr.As16()
	carry := delta
	for i := 15; i >= 0 && carry > 0; i-- {
		carry += uint64(b[i])
		b[i] = byte(carry)
		carry >>= 8
	}
	out := netip.AddrFrom16(b)
	if addr.Is4() {
		out = out.Unmap()
	}
	return out
}
