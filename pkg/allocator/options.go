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

package allocator

import (
	"hash/fnv"
	"math/rand/v2"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
)

// options holds the resolved allocation options; see Option.
type options struct {
	// request identifies the request for allocation or release.
	request Request
	// retry enables the transient-error retry (see isRetryable). Defaulted to
	// true in Allocate; WithRetry(false) turns it off.
	retry bool
	// nodeLock specifies the Kubernetes node name for slice-level ownership (spec.node).
	nodeLock string
}

// Option customizes an Allocate or Release call.
type Option func(*options)

// WithRequest specifies the client Request for an allocation or release.
func WithRequest(req Request) Option {
	return func(op *options) { op.request = req }
}

// WithName specifies a string name as the request.
// Shortcut for WithRequest(Named(name)).
func WithName(name string) Option {
	return WithRequest(Named(name))
}

// WithDeviceRef specifies a DRA ResourceClaim allocated device reference as the request.
// Shortcut for WithRequest(DeviceRef(ref)).
func WithDeviceRef(ref v1alpha1.ResourceClaimDeviceRef) Option {
	return WithRequest(DeviceRef(ref))
}

// WithNode sets the slice node lock (spec.node).
// When specified, the request's node (request.Node()) must match this node,
// otherwise Allocate returns an early validation error before any API calls.
func WithNode(name string) Option { return func(op *options) { op.nodeLock = name } }

// WithRetry toggles the transient-error retry (see isRetryable). It is ON by
// default: Allocate retries lost optimistic-concurrency conflicts and request
// timeouts with the idempotent apply, so a concurrent fill completes without
// errors -- at the cost of latency (the M*S server cost is unchanged; see
// docs/readme.md). Pass WithRetry(false) to fail fast on the first transient
// error instead, e.g. when the caller would rather give up (or try elsewhere)
// than wait out a saturated apiserver.
func WithRetry(enabled bool) Option { return func(op *options) { op.retry = enabled } }

// hashNode returns a 64-bit FNV-1a hash of the node name.
func hashNode(node string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(node))
	return h.Sum64()
}

// visitOrder returns the sub-slice indices [0,n) in the order Allocate should
// try them:
//   - when request.Node() is set: sequential circular order starting from
//     hash(request.Node()) % n, packing allocations on that node's preferred slice;
//   - otherwise: a random permutation across all n slices, spreading allocations
//     uniformly across shared pools.
func (o options) visitOrder(n uint64) []uint64 {
	order := make([]uint64, n)
	reqNode := ""
	if o.request != nil {
		reqNode = o.request.Node()
	}
	if reqNode != "" {
		start := hashNode(reqNode) % n
		for i := range order {
			order[i] = (start + uint64(i)) % n
		}
		return order
	}
	for i := range order {
		order[i] = uint64(i)
	}
	if n > 1 {
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}
	return order
}
