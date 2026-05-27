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
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/common/promise"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// URNBroker provides per-URN promises that fulfill when a resource has been registered. It lets
// multiple Sources running concurrently against the same engine update wait for one another's
// RegisterResource calls without knowing in advance who will produce a given URN.
//
// The intended pattern:
//
//   - Anyone that needs the outputs of a resource by URN calls Get(urn) to receive a promise, then
//     blocks on it via Result(ctx).
//   - The resource monitor calls Resolve(urn, outputs) after each successful registration. The
//     first Resolve for a URN wins; subsequent calls are no-ops.
type URNBroker struct {
	mu       sync.Mutex
	promises map[resource.URN]*promise.CompletionSource[resource.PropertyMap]
}

// NewURNBroker returns a broker with no pending or resolved entries.
func NewURNBroker() *URNBroker {
	return &URNBroker{
		promises: map[resource.URN]*promise.CompletionSource[resource.PropertyMap]{},
	}
}

// entry returns the CompletionSource for urn, creating it if absent. Caller must hold b.mu.
func (b *URNBroker) entry(urn resource.URN) *promise.CompletionSource[resource.PropertyMap] {
	cs, ok := b.promises[urn]
	if !ok {
		cs = &promise.CompletionSource[resource.PropertyMap]{}
		b.promises[urn] = cs
	}
	return cs
}

// Get returns a promise that will be fulfilled with the outputs of the resource registered at the
// given URN. Subsequent Gets for the same URN return the same promise. Safe to call from any
// goroutine.
func (b *URNBroker) Get(urn resource.URN) *promise.Promise[resource.PropertyMap] {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entry(urn).Promise()
}

// Resolve fulfills the promise for the given URN with the supplied outputs. Repeated Resolve calls
// for the same URN are no-ops.
func (b *URNBroker) Resolve(urn resource.URN, outputs resource.PropertyMap) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entry(urn).Fulfill(outputs)
}

// Reject fails the promise for the given URN. Useful for shutdown or to signal that a referenced
// URN will never be registered. Repeated Rejects/Resolves for the same URN are no-ops.
func (b *URNBroker) Reject(urn resource.URN, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entry(urn).Reject(err)
}
