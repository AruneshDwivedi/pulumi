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
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/model"
	hclsyntax "github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/syntax"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pclruntime "github.com/pulumi/pulumi/pkg/v3/pcl/runtime"
	"github.com/pulumi/pulumi/sdk/v3/go/common/promise"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
)

type snippet struct {
	snippet    *resource.Snippet
	loader     schema.ReferenceLoader
	rootDir    string
	workingDir string
	// urn is the URN this snippet will register, pre-computed from the project, stack, type, and name so the
	// broker can mark it Expected and so this source can Reject it on failure (unblocking any cascade waiters).
	urn resource.URN
	// broker is the per-update URN broker; nil if the caller does not need cross-source URN coordination. Used by
	// the snippet to wait on (and resolve traversals of) the resources named in Snippet.References.
	broker *URNBroker

	monitor    pulumirpc.ResourceMonitorClient
	packageRef string
}

// NewSnippetSource creates a Source that registers a single PCL resource snippet. broker, if non-nil, is the
// per-update URN broker the snippet will consult to wait for resources named in s.References. urn is the URN
// this snippet is expected to register; passed in by the caller so the source can Reject it on failure and
// unblock any cascade waiters.
func NewSnippetSource(s resource.Snippet,
	loader schema.ReferenceLoader,
	rootDir, workingDir string,
	urn resource.URN,
	broker *URNBroker,
) func(string) *promise.Promise[struct{}] {
	src := &snippet{
		snippet: &s, loader: loader,
		rootDir: rootDir, workingDir: workingDir,
		urn: urn, broker: broker,
	}
	return src.run
}

func (s *snippet) run(resourceMonitorTarget string) *promise.Promise[struct{}] {
	cts := &promise.CompletionSource[struct{}]{}

	// fail rejects the snippet's outer promise AND its broker entry so that any other snippet waiting on this
	// URN unblocks promptly instead of hanging. Use fail in place of cts.Reject for any failure path that means
	// the snippet's RegisterResource will not run.
	fail := func(err error) {
		if s.broker != nil && s.urn != "" {
			s.broker.Reject(s.urn, err)
		}
		cts.Reject(err)
	}

	go func() {
		// Bind the snippet code.
		input := strings.NewReader(s.snippet.Code)
		parser := hclsyntax.NewParser()
		if err := parser.ParseFile(input, "<snippet>"); err != nil {
			fail(fmt.Errorf("parse input: %w", err))
			return
		}
		if parser.Diagnostics.HasErrors() {
			fail(fmt.Errorf("parse input: %v", parser.Diagnostics))
			return
		}
		contract.Assertf(len(parser.Files) == 1, "Should be one PCL file")
		file := parser.Files[0]

		// Parse every reference URN up front so we can both inject scope variables for the binder and wait on
		// the broker at evaluation time. Each entry maps the HCL identifier used inside the snippet to the URN
		// it resolves to.
		type refEntry struct {
			urn resource.URN
		}
		refs := make(map[string]refEntry, len(s.snippet.References))
		for name, raw := range s.snippet.References {
			urn, err := resource.ParseURN(raw)
			if err != nil {
				fail(fmt.Errorf("invalid URN for reference %q: %w", name, err))
				return
			}
			refs[name] = refEntry{urn: urn}
		}

		// Build the binder-side variables. Today these are typed as DynamicType so the binder accepts any
		// traversal; the runtime SetVariable below provides the actual shape.
		var extras map[string]*model.Variable
		if len(refs) > 0 {
			extras = make(map[string]*model.Variable, len(refs))
			for name := range refs {
				extras[name] = &model.Variable{
					Name:         name,
					VariableType: model.DynamicType,
				}
			}
		}

		// Lookup the resource type in the provider schema and bind the snippet code to it.
		var parameterization *schema.ParameterizationDescriptor
		if s.snippet.Descriptor.Parameterization != nil {
			parameterization = &schema.ParameterizationDescriptor{
				Name:    s.snippet.Descriptor.Parameterization.Name,
				Version: s.snippet.Descriptor.Parameterization.Version,
				Value:   s.snippet.Descriptor.Parameterization.Value,
			}
		}
		descriptor := &schema.PackageDescriptor{
			Name:             s.snippet.Descriptor.Name,
			Version:          s.snippet.Descriptor.Version,
			DownloadURL:      s.snippet.Descriptor.DownloadURL,
			Parameterization: parameterization,
		}

		spec, err := s.loader.LoadPackageReferenceV2(context.TODO(), descriptor)
		if err != nil {
			fail(fmt.Errorf("loading package reference: %w", err))
			return
		}
		res, ok, err := spec.Resources().Get(s.snippet.Type)
		if err != nil {
			fail(fmt.Errorf("getting resource from schema: %w", err))
			return
		}
		if !ok {
			fail(fmt.Errorf("resource type %q not found in package %q", s.snippet.Type, descriptor.Name))
			return
		}

		bindOpts := []pcl.BindOption{pcl.Loader(s.loader)}
		if len(extras) > 0 {
			bindOpts = append(bindOpts, pcl.ExtraScopeVariables(extras))
		}
		attributes, resType, snippetOptions, diags := pcl.BindResource(file, res, bindOpts...)
		if diags.HasErrors() {
			fail(fmt.Errorf("binding resource: %v", diags))
			return
		}

		conn, err := grpc.NewClient(
			resourceMonitorTarget,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			rpcutil.GrpcChannelOptions(),
		)
		if err != nil {
			fail(fmt.Errorf("connect to resource monitor: %w", err))
			return
		}
		defer contract.IgnoreClose(conn)

		monitor := pulumirpc.NewResourceMonitorClient(conn)
		s.monitor = monitor

		// Register the package with the monitor first. The returned ref is what the engine uses to look up the
		// (possibly parameterized) provider for the resource we register below.
		version := ""
		if descriptor.Version != nil {
			version = descriptor.Version.String()
		}
		registerReq := &pulumirpc.RegisterPackageRequest{
			Name:        descriptor.Name,
			Version:     version,
			DownloadUrl: descriptor.DownloadURL,
		}
		if descriptor.Parameterization != nil {
			registerReq.Parameterization = &pulumirpc.Parameterization{
				Name:    descriptor.Parameterization.Name,
				Version: descriptor.Parameterization.Version.String(),
				Value:   descriptor.Parameterization.Value,
			}
		}
		registerResp, err := monitor.RegisterPackage(context.TODO(), registerReq)
		if err != nil {
			fail(fmt.Errorf("register snippet package: %w", err))
			return
		}
		s.packageRef = registerResp.Ref

		infoResp, err := monitor.GetDeploymentInfo(context.TODO(), &emptypb.Empty{})
		if err != nil {
			fail(fmt.Errorf("get deployment info: %w", err))
			return
		}

		evalCtx := pclruntime.NewEvalContext(
			s.workingDir, s.rootDir,
			infoResp.Organization, infoResp.Project, infoResp.Stack,
			nil, s.lookupFunction, nil, s.invoke, nil)

		// Resolve each Snippet.References entry by waiting on the URN broker. Wait in parallel; if any URN's
		// promise rejects, fail the whole snippet. Once all resolve, inject the cty-converted outputs as scope
		// variables so traversals like `comp.value` find them at evaluation time.
		if len(refs) > 0 {
			if s.broker == nil {
				fail(errors.New("snippet has References but no URNBroker is configured"))
				return
			}
			grp, gctx := errgroup.WithContext(context.TODO())
			resolved := make(map[string]resource.PropertyMap, len(refs))
			var resolvedMu sync.Mutex
			for name, ref := range refs {
				grp.Go(func() error {
					outs, err := s.broker.Get(ref.urn).Result(gctx)
					if err != nil {
						return fmt.Errorf("waiting for reference %q (%s): %w", name, ref.urn, err)
					}
					resolvedMu.Lock()
					resolved[name] = outs
					resolvedMu.Unlock()
					return nil
				})
			}
			if err := grp.Wait(); err != nil {
				fail(err)
				return
			}
			for name, outs := range resolved {
				ref := refs[name]
				// Shape the reference as a resource value: {urn, id, ...outputs}, wrapped in an Output marked
				// known with the URN as its dependency. This is what makes `comp.value` traverse like a normal
				// output read and, more importantly, what makes `parent = comp` / `dependsOn = [comp]` /
				// `deletedWith = comp` typecheck and unwrap to the URN — they call UnwrapResource which expects
				// an object with `urn` and `id` fields. The id is not tracked by the broker; we stub it as an
				// empty string the same way the interpreter does for component references.
				obj := make(resource.PropertyMap, len(outs)+2)
				for k, v := range outs {
					obj[k] = v
				}
				obj["urn"] = resource.NewProperty(string(ref.urn))
				if _, ok := obj["id"]; !ok {
					obj["id"] = resource.NewProperty("")
				}
				wrapped := resource.NewProperty(resource.Output{
					Element:      resource.NewProperty(obj),
					Dependencies: []resource.URN{ref.urn},
					Known:        true,
				})
				ctyVal, err := pclruntime.PropertyValueToCty(context.TODO(), nil, wrapped)
				if err != nil {
					fail(fmt.Errorf("converting outputs for reference %q: %w", name, err))
					return
				}
				evalCtx.SetVariable(name, ctyVal)
			}
		}

		props, poison, diags := evalCtx.EvaluateObject(attributes, resType, res.InputProperties)
		if poison != nil {
			fail(fmt.Errorf("snippet evaluation poisoned: %v", *poison))
			return
		}
		if diags.HasErrors() {
			fail(fmt.Errorf("snippet evaluation errors: %v", diags))
			return
		}

		object, err := plugin.MarshalProperties(props, plugin.MarshalOptions{
			Label:         "snippet",
			KeepUnknowns:  true,
			KeepSecrets:   true,
			KeepResources: true,
		})
		if err != nil {
			fail(fmt.Errorf("marshal snippet properties: %w", err))
			return
		}

		registerReqResource := &pulumirpc.RegisterResourceRequest{
			Type:            s.snippet.Type,
			Name:            s.snippet.Name,
			Custom:          true,
			Object:          object,
			PackageRef:      registerResp.GetRef(),
			AcceptSecrets:   true,
			AcceptResources: true,
		}
		if err := applySnippetOptions(evalCtx, snippetOptions, registerReqResource); err != nil {
			fail(fmt.Errorf("snippet options: %w", err))
			return
		}

		_, err = monitor.RegisterResource(context.TODO(), registerReqResource)
		if err != nil {
			fail(fmt.Errorf("register snippet resource: %w", err))
			return
		}
		cts.Fulfill(struct{}{})
	}()
	return cts.Promise()
}

// applySnippetOptions evaluates each non-nil option expression on opts and writes the corresponding field on req.
// Mirrors the resource-options handling in pcl/runtime/interpreter.go's registerResourceWith — a snippet is just a
// resource registration without the surrounding program, so it should accept the same set of options.
func applySnippetOptions(
	evalCtx *pclruntime.EvalContext, opts *pcl.ResourceOptions, req *pulumirpc.RegisterResourceRequest,
) error {
	if opts == nil {
		return nil
	}

	// eval is a small helper that runs an expression and returns the value, treating "poisoned" results as errors
	// (snippets can't propagate poison the way the interpreter can; there's no downstream resource to skip).
	eval := func(name string, expr model.Expression) (resource.PropertyValue, bool, error) {
		v, poison, diags := evalCtx.Evaluate(expr)
		if poison != nil {
			return resource.PropertyValue{}, false, fmt.Errorf("%s: poisoned (%s)", name, *poison)
		}
		if diags.HasErrors() {
			return resource.PropertyValue{}, false, fmt.Errorf("%s: %v", name, diags)
		}
		// Treat null/computed as "unset" so authors can write conditional expressions naturally.
		if v.IsNull() || v.IsComputed() {
			return v, false, nil
		}
		return v, true, nil
	}

	if opts.AdditionalSecretOutputs != nil {
		v, ok, err := eval("additionalSecretOutputs", opts.AdditionalSecretOutputs)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("additionalSecretOutputs must be an array of strings")
			}
			var out []string
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				if !e.IsString() {
					return errors.New("additionalSecretOutputs must be an array of strings")
				}
				out = append(out, e.StringValue())
			}
			req.AdditionalSecretOutputs = out
		}
	}
	if opts.Aliases != nil {
		v, ok, err := eval("aliases", opts.Aliases)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("aliases must be an array of strings or alias objects")
			}
			var aliasOpts []*pulumirpc.Alias
			for _, a := range v.ArrayValue() {
				switch {
				case a.IsString():
					aliasOpts = append(aliasOpts, &pulumirpc.Alias{
						Alias: &pulumirpc.Alias_Urn{Urn: a.StringValue()},
					})
				case a.IsObject():
					obj := a.ObjectValue()
					spec := &pulumirpc.Alias_Spec{}
					setString := func(field resource.PropertyKey, setter func(string)) error {
						attr, ok := obj[field]
						if ok && !attr.IsNull() && !attr.IsComputed() {
							if !attr.IsString() {
								return fmt.Errorf("%s must be a string", field)
							}
							setter(attr.StringValue())
						}
						return nil
					}
					if err := setString("name", func(s string) { spec.Name = s }); err != nil {
						return err
					}
					if err := setString("type", func(s string) { spec.Type = s }); err != nil {
						return err
					}
					if noParent, ok := obj["noParent"]; ok && !noParent.IsNull() && !noParent.IsComputed() {
						if !noParent.IsBool() {
							return errors.New("noParent must be a boolean")
						}
						spec.Parent = &pulumirpc.Alias_Spec_NoParent{NoParent: noParent.BoolValue()}
					}
					if parent, ok := obj["parent"]; ok && !parent.IsNull() && !parent.IsComputed() {
						urn, _, err := pclruntime.UnwrapResource(parent)
						if err != nil {
							return fmt.Errorf("parent: %w", err)
						}
						spec.Parent = &pulumirpc.Alias_Spec_ParentUrn{ParentUrn: urn}
					}
					aliasOpts = append(aliasOpts, &pulumirpc.Alias{Alias: &pulumirpc.Alias_Spec_{Spec: spec}})
				default:
					return errors.New("aliases must be an array of strings or alias objects")
				}
			}
			req.Aliases = aliasOpts
		}
	}
	if opts.CustomTimeouts != nil {
		v, ok, err := eval("customTimeouts", opts.CustomTimeouts)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsObject() {
				return errors.New("customTimeouts must be an object")
			}
			vals := map[string]string{}
			for k, e := range v.ObjectValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				if !e.IsString() {
					return fmt.Errorf("customTimeouts.%s must be a string", k)
				}
				vals[string(k)] = e.StringValue()
			}
			req.CustomTimeouts = &pulumirpc.RegisterResourceRequest_CustomTimeouts{
				Create: vals["create"],
				Update: vals["update"],
				Delete: vals["delete"],
			}
		}
	}
	if opts.DeleteBeforeReplace != nil {
		v, ok, err := eval("deleteBeforeReplace", opts.DeleteBeforeReplace)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsBool() {
				return errors.New("deleteBeforeReplace must be a boolean or null")
			}
			req.DeleteBeforeReplace = v.BoolValue()
			req.DeleteBeforeReplaceDefined = true
		}
	}
	if opts.DeletedWith != nil {
		v, ok, err := eval("deletedWith", opts.DeletedWith)
		if err != nil {
			return err
		}
		if ok {
			urn, _, err := pclruntime.UnwrapResource(v)
			if err != nil {
				return fmt.Errorf("deletedWith: %w", err)
			}
			req.DeletedWith = urn
		}
	}
	if opts.DependsOn != nil {
		v, ok, err := eval("dependsOn", opts.DependsOn)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("dependsOn must be an array of resource objects")
			}
			var deps []string
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				urn, _, err := pclruntime.UnwrapResource(e)
				if err != nil {
					return fmt.Errorf("dependsOn: %w", err)
				}
				deps = append(deps, urn)
			}
			req.Dependencies = append(req.Dependencies, deps...)
		}
	}
	if opts.EnvVarMappings != nil {
		v, ok, err := eval("envVarMappings", opts.EnvVarMappings)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsObject() {
				return errors.New("envVarMappings must be an object mapping env var names to input property keys")
			}
			mappings := map[string]string{}
			for k, e := range v.ObjectValue() {
				if e.IsNull() || e.IsComputed() || !e.IsString() {
					return errors.New("envVarMappings must be an object mapping env var names to input property keys")
				}
				mappings[string(k)] = e.StringValue()
			}
			req.EnvVarMappings = mappings
		}
	}
	if opts.HideDiffs != nil {
		v, ok, err := eval("hideDiffs", opts.HideDiffs)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("hideDiffs must be an array of strings")
			}
			out := []string{}
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				if !e.IsString() {
					return errors.New("hideDiffs must be an array of strings")
				}
				out = append(out, e.StringValue())
			}
			req.HideDiffs = out
		}
	}
	if opts.Hooks != nil {
		v, ok, err := eval("hooks", opts.Hooks)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsObject() {
				return errors.New("hooks must be an object mapping hook types to hook names")
			}
			binding := &pulumirpc.RegisterResourceRequest_ResourceHooksBinding{}
			for hookType, hookList := range v.ObjectValue() {
				if hookList.IsNull() || hookList.IsComputed() {
					continue
				}
				if !hookList.IsArray() {
					return fmt.Errorf("hooks.%s must be an array of hook names", hookType)
				}
				var names []string
				for idx, h := range hookList.ArrayValue() {
					if h.IsNull() || h.IsComputed() {
						continue
					}
					if !h.IsString() {
						return fmt.Errorf("hooks.%s[%d] must be a reference to a named hook", hookType, idx)
					}
					names = append(names, h.StringValue())
				}
				switch hookType {
				case "beforeCreate":
					binding.BeforeCreate = append(binding.BeforeCreate, names...)
				case "afterCreate":
					binding.AfterCreate = append(binding.AfterCreate, names...)
				case "beforeUpdate":
					binding.BeforeUpdate = append(binding.BeforeUpdate, names...)
				case "afterUpdate":
					binding.AfterUpdate = append(binding.AfterUpdate, names...)
				case "beforeDelete":
					binding.BeforeDelete = append(binding.BeforeDelete, names...)
				case "afterDelete":
					binding.AfterDelete = append(binding.AfterDelete, names...)
				default:
					return fmt.Errorf("invalid hook type: %s", hookType)
				}
			}
			if len(binding.BeforeCreate)+len(binding.AfterCreate)+
				len(binding.BeforeUpdate)+len(binding.AfterUpdate)+
				len(binding.BeforeDelete)+len(binding.AfterDelete) > 0 {
				req.Hooks = binding
			}
		}
	}
	if opts.IgnoreChanges != nil {
		v, ok, err := eval("ignoreChanges", opts.IgnoreChanges)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("ignoreChanges must be an array of strings")
			}
			out := []string{}
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				if !e.IsString() {
					return errors.New("ignoreChanges must be an array of strings")
				}
				out = append(out, e.StringValue())
			}
			req.IgnoreChanges = out
		}
	}
	if opts.ImportID != nil {
		v, ok, err := eval("import", opts.ImportID)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsString() {
				return errors.New("import must be a string")
			}
			req.ImportId = v.StringValue()
		}
	}
	if opts.Parent != nil {
		v, ok, err := eval("parent", opts.Parent)
		if err != nil {
			return err
		}
		if ok {
			urn, _, err := pclruntime.UnwrapResource(v)
			if err != nil {
				return fmt.Errorf("parent: %w", err)
			}
			req.Parent = urn
		}
	}
	if opts.PluginDownloadURL != nil {
		v, ok, err := eval("pluginDownloadURL", opts.PluginDownloadURL)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsString() {
				return errors.New("pluginDownloadURL must be a string")
			}
			req.PluginDownloadURL = v.StringValue()
		}
	}
	if opts.Protect != nil {
		v, ok, err := eval("protect", opts.Protect)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsBool() {
				return errors.New("protect must be a boolean or null")
			}
			b := v.BoolValue()
			req.Protect = &b
		}
	}
	if opts.Provider != nil {
		v, ok, err := eval("provider", opts.Provider)
		if err != nil {
			return err
		}
		if ok {
			urn, id, err := pclruntime.UnwrapResource(v)
			if err != nil {
				return fmt.Errorf("provider: %w", err)
			}
			idstr := plugin.UnknownStringValue
			if id.IsString() {
				idstr = id.StringValue()
			}
			req.Provider = fmt.Sprintf("%s::%s", urn, idstr)
		}
	}
	if opts.Providers != nil {
		v, ok, err := eval("providers", opts.Providers)
		if err != nil {
			return err
		}
		if ok {
			// Providers is either a map (pkg name -> provider) or a list of providers keyed by their URN's pkg name.
			ps := map[string]string{}
			switch {
			case v.IsObject():
				for k, e := range v.ObjectValue() {
					urn, id, err := pclruntime.UnwrapResource(e)
					if err != nil {
						return fmt.Errorf("providers: %w", err)
					}
					idstr := plugin.UnknownStringValue
					if id.IsString() {
						idstr = id.StringValue()
					}
					ps[string(k)] = fmt.Sprintf("%s::%s", urn, idstr)
				}
			case v.IsArray():
				for _, e := range v.ArrayValue() {
					urn, id, err := pclruntime.UnwrapResource(e)
					if err != nil {
						return fmt.Errorf("providers: %w", err)
					}
					_, _, pkg, diags := pcl.DecomposeToken(string(resource.URN(urn).Type()), hcl.Range{})
					contract.Assertf(!diags.HasErrors(), "invalid token format from URN %s", urn)
					idstr := plugin.UnknownStringValue
					if id.IsString() {
						idstr = id.StringValue()
					}
					ps[pkg] = fmt.Sprintf("%s::%s", urn, idstr)
				}
			default:
				return errors.New("providers must be an array of provider objects or a map keyed by pkg name")
			}
			req.Providers = ps
		}
	}
	if opts.ReplaceOnChanges != nil {
		v, ok, err := eval("replaceOnChanges", opts.ReplaceOnChanges)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("replaceOnChanges must be an array of strings")
			}
			out := []string{}
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				if !e.IsString() {
					return errors.New("replaceOnChanges must be an array of strings")
				}
				out = append(out, e.StringValue())
			}
			req.ReplaceOnChanges = out
		}
	}
	if opts.ReplaceWith != nil {
		v, ok, err := eval("replaceWith", opts.ReplaceWith)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsArray() {
				return errors.New("replaceWith must be an array of resources")
			}
			var out []string
			for _, e := range v.ArrayValue() {
				if e.IsNull() || e.IsComputed() {
					continue
				}
				urn, _, err := pclruntime.UnwrapResource(e)
				if err != nil {
					return fmt.Errorf("replaceWith: %w", err)
				}
				out = append(out, urn)
			}
			req.ReplaceWith = out
		}
	}
	if opts.ReplacementTrigger != nil {
		// ReplacementTrigger accepts any value (null/computed included) — marshal whatever evaluator returns.
		v, _, diags := evalCtx.Evaluate(opts.ReplacementTrigger)
		if diags.HasErrors() {
			return fmt.Errorf("replacementTrigger: %v", diags)
		}
		marshalled, err := plugin.MarshalPropertyValue("replacementTrigger", v, plugin.MarshalOptions{
			KeepUnknowns: true, KeepSecrets: true, KeepResources: true, KeepOutputValues: true,
		})
		if err != nil {
			return fmt.Errorf("replacementTrigger: %w", err)
		}
		req.ReplacementTrigger = marshalled
	}
	if opts.RetainOnDelete != nil {
		v, ok, err := eval("retainOnDelete", opts.RetainOnDelete)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsBool() {
				return errors.New("retainOnDelete must be a boolean or null")
			}
			b := v.BoolValue()
			req.RetainOnDelete = &b
		}
	}
	if opts.Version != nil {
		v, ok, err := eval("version", opts.Version)
		if err != nil {
			return err
		}
		if ok {
			if !v.IsString() {
				return errors.New("version must be a string")
			}
			req.Version = v.StringValue()
		}
	}
	return nil
}

func (s *snippet) lookupPackageDescriptor(pkg string) *schema.PackageDescriptor {
	// If this is for the snippets package we can return a descriptor directly, else we just guess by name.
	spkg, _, _, _ := pcl.DecomposeToken(s.snippet.Type, hcl.Range{})
	if spkg == pkg {
		desc := &schema.PackageDescriptor{
			Name:        s.snippet.Descriptor.Name,
			Version:     s.snippet.Descriptor.Version,
			DownloadURL: s.snippet.Descriptor.DownloadURL,
		}
		if s.snippet.Descriptor.Parameterization != nil {
			desc.Parameterization = &schema.ParameterizationDescriptor{
				Name:    s.snippet.Descriptor.Parameterization.Name,
				Version: s.snippet.Descriptor.Parameterization.Version,
				Value:   s.snippet.Descriptor.Parameterization.Value,
			}
		}
		return desc
	}

	return &schema.PackageDescriptor{Name: pkg}
}

func (s *snippet) lookupFunction(ctx context.Context, token string) (*schema.Function, error) {
	pkg, mod, typ, diags := pcl.DecomposeToken(token, hcl.Range{})
	contract.Assertf(!diags.HasErrors(), "invalid token format for function token %s", token)

	token = fmt.Sprintf("%s:%s:%s", pkg, mod, typ)

	descriptor := s.lookupPackageDescriptor(pkg)
	pkgref, err := s.loader.LoadPackageReferenceV2(ctx, descriptor)
	if err != nil {
		return nil, fmt.Errorf("load package for token %s: %w", token, err)
	}
	functions := pkgref.Functions()
	schemaFunction, ok, err := functions.Get(token)
	if err != nil {
		return nil, fmt.Errorf("get function from package for token %s: %w", token, err)
	}
	if !ok {
		return nil, fmt.Errorf("get function from package for token %s", token)
	}
	return schemaFunction, nil
}

func (s *snippet) getPackageRefFromToken(token string) (string, error) {
	spkg, _, _, _ := pcl.DecomposeToken(s.snippet.Type, hcl.Range{})
	pkg, _, _, diags := pcl.DecomposeToken(token, hcl.Range{})
	contract.Assertf(!diags.HasErrors(), "invalid token format for resource token %s", token)
	// If the token is for the same package as the snippet, we can return the package ref we got when registering the
	// snippet.
	if pkg == spkg {
		return s.packageRef, nil
	}
	// Else we don't have a ref, just return blank.
	return "", nil
}

func (s *snippet) invoke(
	ctx context.Context, req *pulumirpc.ResourceInvokeRequest,
) (*pulumirpc.InvokeResponse, error) {
	ref, err := s.getPackageRefFromToken(req.Tok)
	if err != nil {
		return nil, err
	}
	req.PackageRef = ref
	resp, err := s.monitor.Invoke(ctx, req)
	return resp, err
}

// NewMuxSource creates a source that runs main alongside zero or more secondary sources, returning a single
// promise that resolves only after every source has finished. Errors from individual sources are joined with
// [errors.Join].
//
// ctx scopes how long the multiplexer is willing to wait. If it is cancelled before every source has resolved,
// the returned promise rejects with the joined errors so far (including ctx.Err()) and stops waiting. The
// individual source goroutines are not interrupted directly; they are expected to react to cancellation through
// their own channels (typically the resource monitor's shutdown signal).
//
// broker is optional. When non-nil, a watchdog goroutine waits for main to resolve and then calls
// [URNBroker.RejectUnresolved] on the broker — at that point the program won't register anything more, so any
// pending URN that no still-running source pre-declared (via [URNBroker.MarkExpected]) is dead and waiting
// snippets are unblocked with a clear error.
func NewMuxSource(
	ctx context.Context,
	broker *URNBroker,
	main func(string) *promise.Promise[struct{}],
	sources ...func(string) *promise.Promise[struct{}],
) func(string) *promise.Promise[struct{}] {
	src := &muxSource{
		ctx:     ctx,
		broker:  broker,
		main:    main,
		sources: sources,
	}
	return src.run
}

type muxSource struct {
	ctx     context.Context //nolint:containedctx // bounded to one engine update; passed by NewMuxSource
	broker  *URNBroker
	main    func(string) *promise.Promise[struct{}]
	sources []func(string) *promise.Promise[struct{}]
}

func (m *muxSource) run(input string) *promise.Promise[struct{}] {
	cts := &promise.CompletionSource[struct{}]{}

	mainP := m.main(input)
	sourceP := make([]*promise.Promise[struct{}], len(m.sources))
	for i, s := range m.sources {
		sourceP[i] = s(input)
	}

	// Watchdog: when the main program finishes (success or failure) it will not register any more resources,
	// so any URN still pending on the broker that no still-running source pre-declared via MarkExpected is
	// dead. Rejecting them wakes up any snippets blocked on those references with a useful error instead of
	// letting them hang forever.
	if m.broker != nil {
		go func() {
			_, _ = mainP.Result(m.ctx)
			m.broker.RejectUnresolved(errors.New("no source registered this URN"))
		}()
	}

	go func() {
		// Block until every promise resolves or our ctx is cancelled. Each waiter is independent so a slow
		// source doesn't delay error reporting for fast ones, but we still wait for all of them to settle
		// before fulfilling so callers don't observe a partial completion.
		errs := make([]error, len(sourceP)+1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mainP.Result(m.ctx); err != nil {
				errs[0] = err
			}
		}()
		for i, p := range sourceP {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := p.Result(m.ctx); err != nil {
					errs[i+1] = err
				}
			}()
		}
		wg.Wait()

		var nonNil []error
		for _, e := range errs {
			if e != nil {
				nonNil = append(nonNil, e)
			}
		}
		switch len(nonNil) {
		case 0:
			cts.Fulfill(struct{}{})
		case 1:
			cts.Reject(nonNil[0])
		default:
			cts.Reject(errors.Join(nonNil...))
		}
	}()

	return cts.Promise()
}
