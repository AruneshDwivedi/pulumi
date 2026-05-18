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

package lifecycletest

import (
	"context"
	"testing"

	"github.com/blang/semver"
	"github.com/gofrs/uuid"

	"github.com/stretchr/testify/require"

	. "github.com/pulumi/pulumi/pkg/v3/engine" //nolint:revive
	lt "github.com/pulumi/pulumi/pkg/v3/engine/lifecycletest/framework"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy/deploytest"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

// TestPclSnippet checks that we can run a PCL snippet via an engine update.
func TestPclSnippet(t *testing.T) {
	t.Parallel()

	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				CreateF: func(ctx context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{
						ID:         resource.ID(id),
						Properties: cr.Properties,
					}, nil
				},
			}, nil
		}),
	}

	snippets := []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       ``,
		},
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, monitor *deploytest.ResourceMonitor) error {
		return nil
	})

	hostF := deploytest.NewPluginHostF(nil, nil, programF, loaders...)
	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            hostF,
		},
	}

	// Make an empty snapshot.
	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	// Add a snippet to the snapshot and rerun the update to execute it.
	snap.Snippets = snippets
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)

	// Check that the snippet is still present in the snapshot.
	require.Len(t, snap.Snippets, 1)
	require.Equal(t, `test-resource`, snap.Snippets[0].Name)
	require.Equal(t, `pkgA:index:res`, snap.Snippets[0].Type)
	require.Equal(t, `{}`, snap.Snippets[0].Code)

	// Check the resource was created.
	require.Len(t, snap.Resources, 2)
	require.Equal(t, tokens.Type("pulumi:providers:pkgA"), snap.Resources[0].Type)
	require.Equal(t, tokens.Type("pkgA:index:res"), snap.Resources[1].Type)
}

// TestPclInvalidSnippet checks that an invalid snippet (i.e does not type check) returns an error.
func TestPclInvalidSnippet(t *testing.T) {
	t.Parallel()

	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(ctx context.Context, gsr plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(`{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "propA": { "type": "boolean" },
        "propB": { "type": "string" }
      },
      "requiredInputs": ["propA", "propB"]
    }
  }
}`)}, nil
				},
				CreateF: func(ctx context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{
						ID:         resource.ID(id),
						Properties: cr.Properties,
					}, nil
				},
			}, nil
		}),
	}

	snippets := []resource.Snippet{
		// only set one property
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `propA = true`,
		},
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, monitor *deploytest.ResourceMonitor) error {
		return nil
	})

	hostF := deploytest.NewPluginHostF(nil, nil, programF, loaders...)
	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            hostF,
		},
	}

	// Make an empty snapshot.
	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	// Add a snippet to the snapshot and rerun the update to execute it.
	snap.Snippets = snippets
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.ErrorContains(t, err, "stuff")
}
