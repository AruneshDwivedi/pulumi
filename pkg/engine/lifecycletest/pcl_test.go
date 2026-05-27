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

// pclSnippetTestProvider returns a loader for a single-resource "pkgA" provider whose schema is
// passed in by the caller. The provider counts create/update/delete calls into the supplied slices.
func pclSnippetTestProvider(
	schema string, created, updated, deleted *[]resource.URN,
) []*deploytest.ProviderLoader {
	return []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(ctx context.Context, _ plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(schema)}, nil
				},
				CreateF: func(ctx context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					if created != nil {
						*created = append(*created, cr.URN)
					}
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{ID: resource.ID(id), Properties: cr.Properties}, nil
				},
				DiffF: func(_ context.Context, req plugin.DiffRequest) (plugin.DiffResult, error) {
					if !req.OldInputs.DeepEquals(req.NewInputs) {
						return plugin.DiffResult{Changes: plugin.DiffSome}, nil
					}
					return plugin.DiffResult{}, nil
				},
				UpdateF: func(_ context.Context, req plugin.UpdateRequest) (plugin.UpdateResponse, error) {
					if updated != nil {
						*updated = append(*updated, req.URN)
					}
					return plugin.UpdateResponse{Properties: req.NewInputs, Status: resource.StatusOK}, nil
				},
				DeleteF: func(_ context.Context, req plugin.DeleteRequest) (plugin.DeleteResponse, error) {
					if deleted != nil {
						*deleted = append(*deleted, req.URN)
					}
					return plugin.DeleteResponse{Status: resource.StatusOK}, nil
				},
			}, nil
		}),
	}
}

const pclSnippetSchemaPropA = `{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "propA": { "type": "boolean" }
      },
      "requiredInputs": ["propA"]
    }
  }
}`

// TestPclSnippet checks that we can run a PCL snippet via an engine update.
func TestPclSnippet(t *testing.T) {
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
        "propA": { "type": "boolean" }
      },
      "requiredInputs": ["propA"]
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
	require.NoError(t, err)

	// Check that the snippet is still present in the snapshot.
	require.Len(t, snap.Snippets, 1)
	require.Equal(t, `test-resource`, snap.Snippets[0].Name)
	require.Equal(t, `pkgA:index:res`, snap.Snippets[0].Type)
	require.Equal(t, `propA = true`, snap.Snippets[0].Code)

	// Check the resource was created.
	require.Len(t, snap.Resources, 2)
	require.Equal(t, tokens.Type("pulumi:providers:pkgA"), snap.Resources[0].Type)
	require.Equal(t, tokens.Type("pkgA:index:res"), snap.Resources[1].Type)
	require.Equal(t, resource.PropertyMap{"propA": resource.NewProperty(true)}, snap.Resources[1].Inputs)
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
	_, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.ErrorContains(t, err, "Missing required input \"propB\"")
}

// TestPclSnippetUpdate checks that mutating the code on a snippet between updates causes the
// underlying resource to be updated rather than recreated.
func TestPclSnippetUpdate(t *testing.T) {
	t.Parallel()

	var created, updated, deleted []resource.URN
	loaders := pclSnippetTestProvider(pclSnippetSchemaPropA, &created, &updated, &deleted)

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})
	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `propA = true`,
		},
	}
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)
	require.Len(t, created, 1)
	require.Empty(t, updated)
	require.Equal(t, resource.PropertyMap{"propA": resource.NewProperty(true)}, snap.Resources[1].Inputs)

	// Change the snippet code and rerun: we expect an Update, not a Create or Delete.
	snap.Snippets[0].Code = `propA = false`
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "2")
	require.NoError(t, err)
	require.Len(t, created, 1, "resource should not be recreated")
	require.Len(t, updated, 1, "resource should be updated once")
	require.Empty(t, deleted)
	require.Equal(t, resource.PropertyMap{"propA": resource.NewProperty(false)}, snap.Resources[1].Inputs)
}

// TestPclSnippetDelete checks that removing a snippet from the snapshot causes the engine to
// delete the resource it had previously synthesized.
func TestPclSnippetDelete(t *testing.T) {
	t.Parallel()

	var created, updated, deleted []resource.URN
	loaders := pclSnippetTestProvider(pclSnippetSchemaPropA, &created, &updated, &deleted)

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})
	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `propA = true`,
		},
	}
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)
	require.Len(t, snap.Resources, 2)
	require.Len(t, created, 1)

	// Remove the snippet and rerun. The synthesized resource should be deleted; the default
	// provider for pkgA should also disappear since nothing references it.
	snap.Snippets = nil
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "2")
	require.NoError(t, err)
	require.Empty(t, snap.Snippets)
	require.Empty(t, updated)
	require.Len(t, deleted, 1)
	for _, r := range snap.Resources {
		require.NotEqual(t, tokens.Type("pkgA:index:res"), r.Type, "snippet resource should be gone")
	}
}

// TestPclMultipleSnippets checks that several snippets in a snapshot each produce their own
// resource and that all of them survive a round trip.
func TestPclMultipleSnippets(t *testing.T) {
	t.Parallel()

	loaders := pclSnippetTestProvider(pclSnippetSchemaPropA, nil, nil, nil)
	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})
	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "r1", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `propA = true`,
		},
		{
			Name: "r2", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `propA = false`,
		},
	}
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)
	require.Len(t, snap.Snippets, 2)

	// Index resources by URN name so the assertions don't depend on engine ordering.
	resourcesByName := map[string]*resource.State{}
	for _, r := range snap.Resources {
		if r.Type == "pkgA:index:res" {
			resourcesByName[r.URN.Name()] = r
		}
	}
	require.Len(t, resourcesByName, 2)
	require.Equal(t,
		resource.PropertyMap{"propA": resource.NewProperty(true)},
		resourcesByName["r1"].Inputs)
	require.Equal(t,
		resource.PropertyMap{"propA": resource.NewProperty(false)},
		resourcesByName["r2"].Inputs)
}

// TestPclSnippetBuiltins exercises the PCL evaluator wired up for a snippet, verifying that it has
// access to context (project, stack, organization) and trivial builtins (max, length).
func TestPclSnippetBuiltins(t *testing.T) {
	t.Parallel()

	schemaJSON := `{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "projectName":  { "type": "string" },
        "stackName":    { "type": "string" },
        "orgName":      { "type": "string" },
        "maxValue":     { "type": "integer" },
        "lengthValue":  { "type": "integer" }
      },
      "requiredInputs": [
        "projectName", "stackName", "orgName",
        "maxValue", "lengthValue"
      ]
    }
  },
  "functions": {
    "pkgA:index:echo": {
      "inputs": {
        "properties": {
          "input": { "type": "string" }
        }
      },
      "outputs": {
        "properties": {
          "result": { "type": "string" }
        },
        "required": ["result"]
      }
    }
  }
}`

	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(_ context.Context, _ plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(schemaJSON)}, nil
				},
				CreateF: func(_ context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{ID: resource.ID(id), Properties: cr.Properties}, nil
				},
			}, nil
		}),
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})

	p := &lt.TestPlan{
		Project: "my-project",
		Stack:   "my-stack",
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code: `projectName = project()
stackName = stack()
orgName = organization()
maxValue = max(1, 7, 3)
lengthValue = length(["a", "b", "c", "d"])`,
		},
	}

	target := p.GetTarget(t, snap)
	target.Organization = "my-org"
	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), target, p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)

	// Find the synthesized resource.
	var res *resource.State
	for _, r := range snap.Resources {
		if r.Type == "pkgA:index:res" {
			res = r
			break
		}
	}
	require.NotNil(t, res, "snippet resource should have been created")
	require.Equal(t, resource.PropertyMap{
		"projectName": resource.NewProperty("my-project"),
		"stackName":   resource.NewProperty("my-stack"),
		"orgName":     resource.NewProperty("my-org"),
		"maxValue":    resource.NewProperty(7.0),
		"lengthValue": resource.NewProperty(4.0),
	}, res.Inputs)
}

// TestPclSnippetDirectories checks that the cwd() and rootDirectory() builtins are wired through to
// the snippet evaluator. In the lifecycle test framework the project root is "/", so both should
// resolve to that.
func TestPclSnippetDirectories(t *testing.T) {
	t.Parallel()

	schemaJSON := `{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "workingDir": { "type": "string" },
        "rootDir":    { "type": "string" }
      },
      "requiredInputs": ["workingDir", "rootDir"]
    }
  }
}`

	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(_ context.Context, _ plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(schemaJSON)}, nil
				},
				CreateF: func(_ context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{ID: resource.ID(id), Properties: cr.Properties}, nil
				},
			}, nil
		}),
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})

	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code: `workingDir = cwd()
rootDir = rootDirectory()`,
		},
	}

	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)

	var res *resource.State
	for _, r := range snap.Resources {
		if r.Type == "pkgA:index:res" {
			res = r
			break
		}
	}
	require.NotNil(t, res, "snippet resource should have been created")
	require.Equal(t, resource.PropertyMap{
		"workingDir": resource.NewProperty("/"),
		"rootDir":    resource.NewProperty("/"),
	}, res.Inputs)
}

// TestPclSnippetInvoke checks that a snippet can call an invoke against the resource monitor and use
// the returned value as a resource input.
func TestPclSnippetInvoke(t *testing.T) {
	t.Parallel()

	schemaJSON := `{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "message": { "type": "string" }
      },
      "requiredInputs": ["message"]
    }
  },
  "functions": {
    "pkgA:index:echo": {
      "inputs": {
        "properties": {
          "input": { "type": "string" }
        },
        "required": ["input"]
      },
      "outputs": {
        "properties": {
          "result": { "type": "string" }
        },
        "required": ["result"]
      }
    }
  }
}`

	var invoked []string
	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(_ context.Context, _ plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(schemaJSON)}, nil
				},
				CreateF: func(_ context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{ID: resource.ID(id), Properties: cr.Properties}, nil
				},
				InvokeF: func(_ context.Context, req plugin.InvokeRequest) (plugin.InvokeResponse, error) {
					input := req.Args["input"].StringValue()
					invoked = append(invoked, input)
					return plugin.InvokeResponse{
						Properties: resource.PropertyMap{
							"result": resource.NewProperty("echoed: " + input),
						},
					}, nil
				},
			}, nil
		}),
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		return nil
	})

	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			Code:       `message = invoke("pkgA:index:echo", { input = "hi" }).result`,
		},
	}

	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)

	require.Equal(t, []string{"hi"}, invoked, "invoke should be called exactly once with input \"hi\"")

	var res *resource.State
	for _, r := range snap.Resources {
		if r.Type == "pkgA:index:res" {
			res = r
			break
		}
	}
	require.NotNil(t, res, "snippet resource should have been created")
	require.Equal(t, resource.PropertyMap{
		"message": resource.NewProperty("echoed: hi"),
	}, res.Inputs)
}

// TestPclSnippetResourceReference checks that a snippet can reference a resource registered by the
// main program by its logical name (declared in Snippet.References) and read one of its output
// properties as an input to the snippet's own resource.
func TestPclSnippetResourceReference(t *testing.T) {
	t.Parallel()

	schemaJSON := `{
  "version": "0.0.1",
  "name": "pkgA",
  "resources": {
    "pkgA:index:res": {
      "inputProperties": {
        "message": { "type": "string" }
      },
      "requiredInputs": ["message"]
    },
    "pkgA:index:Comp": {
      "isComponent": true,
      "inputProperties": {
        "value": { "type": "string" }
      },
      "requiredInputs": ["value"],
      "properties": {
        "value": { "type": "string" }
      },
      "required": ["value"]
    }
  }
}`

	loaders := []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader("pkgA", semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return &deploytest.Provider{
				GetSchemaF: func(_ context.Context, _ plugin.GetSchemaRequest) (plugin.GetSchemaResponse, error) {
					return plugin.GetSchemaResponse{Schema: []byte(schemaJSON)}, nil
				},
				CreateF: func(_ context.Context, cr plugin.CreateRequest) (plugin.CreateResponse, error) {
					uuid, err := uuid.NewV4()
					if err != nil {
						return plugin.CreateResponse{}, err
					}
					id := uuid.String()
					if cr.Preview {
						id = ""
					}
					return plugin.CreateResponse{ID: resource.ID(id), Properties: cr.Properties}, nil
				},
				ConstructF: func(
					_ context.Context, req plugin.ConstructRequest, _ *deploytest.ResourceMonitor,
				) (plugin.ConstructResponse, error) {
					return plugin.ConstructResponse{
						URN:     resource.URN("urn:pulumi:test::test::pkgA:index:Comp::comp"),
						Outputs: resource.PropertyMap{"value": req.Inputs["value"]},
					}, nil
				},
			}, nil
		}),
	}

	programF := deploytest.NewLanguageRuntimeF(func(_ plugin.RunInfo, monitor *deploytest.ResourceMonitor) error {
		_, err := monitor.RegisterResource("pkgA:index:Comp", "comp", false, deploytest.ResourceOptions{
			Remote: true,
			Inputs: resource.PropertyMap{"value": resource.NewProperty("hello")},
		})
		return err
	})

	p := &lt.TestPlan{
		Options: lt.TestUpdateOptions{
			SkipDisplayTests: true,
			T:                t,
			HostF:            deploytest.NewPluginHostF(nil, nil, programF, loaders...),
		},
	}

	snap, err := lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, nil), p.Options, false, p.BackendClient, nil, "0")
	require.NoError(t, err)

	snap.Snippets = []resource.Snippet{
		{
			Name: "test-resource", Type: "pkgA:index:res",
			Descriptor: resource.PackageDescriptor{Name: "pkgA"},
			References: map[string]string{
				"comp": "urn:pulumi:test::test::pkgA:index:Comp::comp",
			},
			Code: `message = comp.value`,
		},
	}

	snap, err = lt.TestOp(Update).RunStep(
		p.GetProject(), p.GetTarget(t, snap), p.Options, false, p.BackendClient, nil, "1")
	require.NoError(t, err)

	var res *resource.State
	for _, r := range snap.Resources {
		if r.Type == "pkgA:index:res" {
			res = r
			break
		}
	}
	require.NotNil(t, res, "snippet resource should have been created")
	require.Equal(t, resource.PropertyMap{
		"message": resource.NewProperty("hello"),
	}, res.Inputs)
}
