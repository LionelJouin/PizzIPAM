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

package naming_test

import (
	"testing"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	"github.com/lioneljouin/pizzipam/pkg/naming"
)

func ptr(s string) *string { return &s }

func TestName(t *testing.T) {
	tests := []struct {
		name string
		spec v1alpha1.IPSliceSpec
		want string
	}{
		{
			name: "IPv4, no namespace",
			spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network"},
				SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.0.0", PrefixLength: 26},
			},
			want: "abc.blue-network.ipv4-192-168-0-0-26",
		},
		{
			name: "IPv4, with namespace",
			spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network", Namespace: ptr("team-a")},
				SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.0.64", PrefixLength: 26},
			},
			want: "abc.blue-network.team-a.ipv4-192-168-0-64-26",
		},
		{
			name: "IPv4, empty namespace pointer behaves like no namespace",
			spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network", Namespace: ptr("")},
				SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.0.0", PrefixLength: 26},
			},
			want: "abc.blue-network.ipv4-192-168-0-0-26",
		},
		{
			name: "IPv6",
			spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network"},
				SliceSubnet:   v1alpha1.Subnet{Family: "IPv6", Prefix: "fe80::", PrefixLength: 122},
			},
			want: "abc.blue-network.ipv6-fe80---122",
		},
		{
			name: "IPv6, non-canonical prefix is canonicalized",
			spec: v1alpha1.IPSliceSpec{
				PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network"},
				SliceSubnet:   v1alpha1.Subnet{Family: "IPv6", Prefix: "FE80:0:0:0:0:0:0:0", PrefixLength: 122},
			},
			want: "abc.blue-network.ipv6-fe80---122",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := naming.Name(tt.spec); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNameIsDeterministic guards the property the server relies on: the same
// identity always yields the same name (so a client can address a block blind).
func TestNameIsDeterministic(t *testing.T) {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Name: "abc", Kind: "blue-network"},
		SliceSubnet:   v1alpha1.Subnet{Family: "IPv4", Prefix: "192.168.0.0", PrefixLength: 26},
	}
	if naming.Name(spec) != naming.Name(spec) {
		t.Fatal("Name() is not deterministic for identical input")
	}
}
