// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deploy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

func TestURNBroker_ResolveBeforeGet(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	outputs := resource.PropertyMap{"k": resource.NewProperty("v")}

	b.Resolve(urn, outputs)

	got, err := b.Get(urn).Result(t.Context())
	require.NoError(t, err)
	require.Equal(t, outputs, got)
}

func TestURNBroker_GetBeforeResolve(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	outputs := resource.PropertyMap{"k": resource.NewProperty("v")}

	p := b.Get(urn)

	// Resolve from another goroutine; Result should unblock.
	go b.Resolve(urn, outputs)

	got, err := p.Result(t.Context())
	require.NoError(t, err)
	require.Equal(t, outputs, got)
}

func TestURNBroker_GetReturnsSamePromise(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	p1 := b.Get(urn)
	p2 := b.Get(urn)
	require.Same(t, p1, p2, "repeated Gets for the same URN should return the same promise")
}

func TestURNBroker_FirstResolveWins(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	first := resource.PropertyMap{"k": resource.NewProperty("first")}
	second := resource.PropertyMap{"k": resource.NewProperty("second")}

	b.Resolve(urn, first)
	b.Resolve(urn, second)

	got, err := b.Get(urn).Result(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, got, "the first Resolve should win; later ones are no-ops")
}

func TestURNBroker_Reject(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	rejErr := errors.New("nope")

	p := b.Get(urn)
	b.Reject(urn, rejErr)

	_, err := p.Result(t.Context())
	require.ErrorIs(t, err, rejErr)
}

// TestURNBroker_ConcurrentGetsAndResolves stresses the broker with many goroutines all racing to
// Get and Resolve the same set of URNs, asserting every Get observes the resolved value.
func TestURNBroker_ConcurrentGetsAndResolves(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	const urns = 50
	const gettersPerURN = 10

	all := make([]resource.URN, urns)
	want := make(map[resource.URN]resource.PropertyMap, urns)
	for i := range urns {
		urn := resource.URN("urn:pulumi:s::p::pkg:typ::" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		all[i] = urn
		want[urn] = resource.PropertyMap{"i": resource.NewProperty(float64(i))}
	}

	var wg sync.WaitGroup
	ctx := t.Context()

	for _, urn := range all {
		for range gettersPerURN {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := b.Get(urn).Result(ctx)
				require.NoError(t, err)
				require.Equal(t, want[urn], got)
			}()
		}
	}

	// Resolve every URN from a separate goroutine.
	for _, urn := range all {
		go b.Resolve(urn, want[urn])
	}

	wg.Wait()
}

// TestURNBroker_ResolveDoesNotBlock checks that Resolve does not block even if no one has Got the
// URN yet; the resolved outputs should be observable by later Gets.
func TestURNBroker_ResolveDoesNotBlock(t *testing.T) {
	t.Parallel()
	b := NewURNBroker()

	urn := resource.URN("urn:pulumi:s::p::pkg:typ::a")
	outputs := resource.PropertyMap{"k": resource.NewProperty("v")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Resolve(urn, outputs)
	}()

	select {
	case <-done:
	case <-context.Background().Done():
		t.Fatal("Resolve should not block")
	}

	got, err := b.Get(urn).Result(t.Context())
	require.NoError(t, err)
	require.Equal(t, outputs, got)
}
