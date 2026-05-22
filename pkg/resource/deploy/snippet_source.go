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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hashicorp/hcl/v2"
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

	monitor    pulumirpc.ResourceMonitorClient
	packageRef string
}

// NewSnippetSource creates a Source that registers a single PCL resource snippet.
func NewSnippetSource(s resource.Snippet,
	loader schema.ReferenceLoader,
	rootDir, workingDir string,
) func(string) *promise.Promise[struct{}] {
	src := &snippet{snippet: &s, loader: loader, rootDir: rootDir, workingDir: workingDir}
	return src.run
}

// Next drives the snippet iterator through a two-step protocol:
//
//  1. First call: emits a default provider registration for the snippet's package and returns
//     immediately. The done channel is stored so we can read the provider result later.
//
//  2. Second call: blocks until the provider registration completes, then emits the resource
//     registration event with the provider reference filled in.
//
//  3. Third call (and beyond): returns nil to signal that the iterator is exhausted.
func (s *snippet) run(resourceMonitorTarget string) *promise.Promise[struct{}] {
	// Bind the snippet code
	input := strings.NewReader(s.snippet.Code)
	parser := hclsyntax.NewParser()
	if err := parser.ParseFile(input, "<snippet>"); err != nil {
		return promise.Errorf[struct{}]("parse input: %w", err)
	}
	if parser.Diagnostics.HasErrors() {
		return promise.Errorf[struct{}]("parse input: %v", parser.Diagnostics)
	}
	contract.Assertf(len(parser.Files) == 1, "Should be one PCL file")
	file := parser.Files[0]

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
		return promise.Errorf[struct{}]("loading package reference: %w", err)
	}
	res, ok, err := spec.Resources().Get(s.snippet.Type)
	if err != nil {
		return promise.Errorf[struct{}]("getting resource from schema: %w", err)
	}
	if !ok {
		return promise.Errorf[struct{}]("resource type %q not found in package %q", s.snippet.Type, descriptor.Name)
	}

	attributes, resType, diags := pcl.BindResource(file, res, pcl.Loader(s.loader))
	if diags.HasErrors() {
		return promise.Errorf[struct{}]("binding resource: %v", diags)
	}

	cts := &promise.CompletionSource[struct{}]{}
	go func() {
		conn, err := grpc.NewClient(
			resourceMonitorTarget,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			rpcutil.GrpcChannelOptions(),
		)
		if err != nil {
			cts.Reject(fmt.Errorf("connect to resource monitor: %w", err))
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
			cts.Reject(fmt.Errorf("register snippet package: %w", err))
			return
		}
		s.packageRef = registerResp.Ref

		infoResp, err := monitor.GetDeploymentInfo(context.TODO(), &emptypb.Empty{})
		if err != nil {
			cts.Reject(fmt.Errorf("get deployment info: %w", err))
			return
		}

		evalCtx := pclruntime.NewEvalContext(
			s.workingDir, s.rootDir,
			infoResp.Organization, infoResp.Project, infoResp.Stack,
			nil, s.lookupFunction, nil, s.invoke, nil)
		props, poison, diags := evalCtx.EvaluateObject(attributes, resType, res.InputProperties)
		if poison != nil {
			cts.Reject(fmt.Errorf("snippet evaluation poisoned: %v", *poison))
			return
		}
		if diags.HasErrors() {
			cts.Reject(fmt.Errorf("snippet evaluation errors: %v", diags))
			return
		}

		object, err := plugin.MarshalProperties(props, plugin.MarshalOptions{
			Label:         "snippet",
			KeepUnknowns:  true,
			KeepSecrets:   true,
			KeepResources: true,
		})
		if err != nil {
			cts.Reject(fmt.Errorf("marshal snippet properties: %w", err))
			return
		}

		_, err = monitor.RegisterResource(context.TODO(), &pulumirpc.RegisterResourceRequest{
			Type:            s.snippet.Type,
			Name:            s.snippet.Name,
			Custom:          true,
			Object:          object,
			PackageRef:      registerResp.GetRef(),
			AcceptSecrets:   true,
			AcceptResources: true,
		})
		if err != nil {
			cts.Reject(fmt.Errorf("register snippet resource: %w", err))
			return
		}
		cts.Fulfill(struct{}{})
	}()
	return cts.Promise()
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
	pkg, _, _, diags := pcl.DecomposeToken(token, hcl.Range{})
	contract.Assertf(!diags.HasErrors(), "invalid token format for resource token %s", token)
	// If the token is for the same package as the snippet, we can return the package ref we got when registering the
	// snippet.
	if pkg == s.snippet.Descriptor.Name {
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

// MuxSource creates a source that multiplexes the given sources, interleaving their events
// and returning nil only when all sources are exhausted. This is used to run PCL snippets
// alongside the main program source.
func NewMuxSource(
	main func(string) *promise.Promise[struct{}],
	sources ...func(string) *promise.Promise[struct{}],
) func(string) *promise.Promise[struct{}] {
	src := &muxSource{
		main:    main,
		sources: sources,
	}
	return src.run
}

type muxSource struct {
	main    func(string) *promise.Promise[struct{}]
	sources []func(string) *promise.Promise[struct{}]
}

func (m *muxSource) run(input string) *promise.Promise[struct{}] {
	cts := &promise.CompletionSource[struct{}]{}

	ps := make([]*promise.Promise[struct{}], 1+len(m.sources))
	ps[0] = m.main(input)
	for i, src := range m.sources {
		ps[i+1] = src(input)
	}

	go func() {
		for {
			// Wait for all the sources to complete.
			allDone := true
			for _, p := range ps {
				_, _, ok := p.TryResult()
				if !ok {
					allDone = false
				}
			}
			if allDone {
				break
			}
		}
		var errs []error
		for _, p := range ps {
			_, err, ok := p.TryResult()
			contract.Assertf(ok, "All promises should be done at this point")
			if err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == 0 {
			cts.Fulfill(struct{}{})
		} else if len(errs) == 1 {
			cts.Reject(errs[0])
		} else {
			cts.Reject(errors.Join(errs...))
		}
	}()

	return cts.Promise()
}
