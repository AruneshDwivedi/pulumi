// Copyright 2018, Pulumi Corporation.
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi/pkg/v3/secrets/b64"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/promise"
	sdkproviders "github.com/pulumi/pulumi/sdk/v3/go/common/providers"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/version"
)

func newResource(name string) *resource.State {
	ty := tokens.Type("test")
	return &resource.State{
		Type:    ty,
		URN:     resource.NewURN(tokens.QName("teststack"), tokens.PackageName("pkg"), ty, ty, name),
		Inputs:  make(resource.PropertyMap),
		Outputs: make(resource.PropertyMap),
	}
}

func newSnapshot(resources []*resource.State, ops []resource.Operation) *Snapshot {
	return NewSnapshot(Manifest{
		Time:    time.Now(),
		Version: version.Version,
		Plugins: nil,
	}, b64.NewBase64SecretsManager(), resources, ops, SnapshotMetadata{}, nil)
}

func TestPendingOperationsDeployment(t *testing.T) {
	t.Parallel()

	resourceA := newResource("a")
	resourceB := newResource("b")
	snap := newSnapshot([]*resource.State{
		resourceA,
	}, []resource.Operation{
		{
			Type:     resource.OperationTypeCreating,
			Resource: resourceB,
		},
	})

	_, err := NewDeployment(&plugin.Context{}, &Options{}, nil, &Target{}, snap, nil, NewNullSource("test"), nil, nil)
	require.NoError(t, err)
}

func TestGlobUrn(t *testing.T) {
	t.Parallel()

	globs := []struct {
		input      string
		expected   []resource.URN
		unexpected []resource.URN
	}{
		{
			input: "**",
			expected: []resource.URN{
				"urn:pulumi:stack::test::typ$aws:resource::aname",
				"urn:pulumi:stack::test::typ$aws:resource::bar",
				"urn:pulumi:stack::test::typ$azure:resource::bar",
			},
		},
		{
			input: "urn:pulumi:stack::test::typ*:resource::bar",
			expected: []resource.URN{
				"urn:pulumi:stack::test::typ$aws:resource::bar",
				"urn:pulumi:stack::test::typ$azure:resource::bar",
			},
			unexpected: []resource.URN{
				"urn:pulumi:stack::test::ty:resource::bar",
				"urn:pulumi:stack::test::type:resource::foobar",
			},
		},
		{
			input:      "**:aname",
			expected:   []resource.URN{"urn:pulumi:stack::test::typ$aws:resource::aname"},
			unexpected: []resource.URN{"urn:pulumi:stack::test::typ$aws:resource::somename"},
		},
		{
			input: "*:*:stack::test::typ$aws:resource::*",
			expected: []resource.URN{
				"urn:pulumi:stack::test::typ$aws:resource::aname",
				"urn:pulumi:stack::test::typ$aws:resource::bar",
			},
			unexpected: []resource.URN{
				"urn:pulumi:stack::test::typ$azure:resource::aname",
			},
		},
		{
			input:    "stack::test::typ$aws:resource::none",
			expected: []resource.URN{"stack::test::typ$aws:resource::none"},
			unexpected: []resource.URN{
				"stack::test::typ$aws:resource::nonee",
			},
		},
	}
	for _, tt := range globs {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			targets := NewUrnTargets([]string{tt.input})
			for _, urn := range tt.expected {
				assert.True(t, targets.Contains(urn))
			}
		})
	}
}

func makeProviderRef(t *testing.T, name string) sdkproviders.Reference {
	t.Helper()
	providerURN := resource.URN("urn:pulumi:stack::project::pulumi:providers:" + name + "::default")
	ref, err := sdkproviders.NewReference(providerURN, resource.ID("id-"+name))
	require.NoError(t, err)
	return ref
}

func TestLookupOrRegisterExtension(t *testing.T) {
	t.Parallel()

	d := &Deployment{
		extensions: map[sdkproviders.Reference][]inFlightExtension{},
	}
	provA := makeProviderRef(t, "k8s")
	provB := makeProviderRef(t, "azure")
	extensionA := apitype.ExtensionRef("extension-a")
	extensionB := apitype.ExtensionRef("extension-b")

	// sentinelStep is a stand-in for an emitted parameterize step. The helper only
	// returns whatever createStep returns; tests don't actually execute it.
	type sentinelStep struct{ Step }
	sentinel := &sentinelStep{}

	var capturedCS *promise.CompletionSource[struct{}]
	createCalls := 0
	create := func(cs *promise.CompletionSource[struct{}]) Step {
		createCalls++
		capturedCS = cs
		return sentinel
	}

	// First call: nothing registered yet -> createStep is invoked and its step returned.
	promise1, step1 := d.LookupOrRegisterExtension(provA, extensionA, create)
	require.Equal(t, 1, createCalls)
	require.Same(t, Step(sentinel), step1, "first call should hand back the constructed step")
	require.NotNil(t, promise1)
	require.NotNil(t, capturedCS)

	// Second call for the same (provider, ref): createStep must not run again,
	// the returned step is nil, but the promise is the same one tied to capturedCS.
	promise2, step2 := d.LookupOrRegisterExtension(provA, extensionA, create)
	require.Equal(t, 1, createCalls, "duplicate registration must not invoke createStep again")
	require.Nil(t, step2, "duplicate registration must not return a step")
	require.NotNil(t, promise2)

	// Fulfilling the original CompletionSource resolves the second caller's promise.
	capturedCS.MustFulfill(struct{}{})
	_, err := promise2.Result(t.Context())
	require.NoError(t, err)

	// Different ref under the same provider -> independent.
	_, stepY := d.LookupOrRegisterExtension(provA, extensionB, create)
	require.Equal(t, 2, createCalls)
	require.Same(t, Step(sentinel), stepY)

	// Same ref under a different provider -> independent.
	_, stepB := d.LookupOrRegisterExtension(provB, extensionA, create)
	require.Equal(t, 3, createCalls)
	require.Same(t, Step(sentinel), stepB)
}
