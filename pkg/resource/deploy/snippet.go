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

	hclsyntax "github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/syntax"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pclruntime "github.com/pulumi/pulumi/pkg/v3/pcl/runtime"
	sdkproviders "github.com/pulumi/pulumi/sdk/v3/go/common/providers"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
)

type snippetState int

const (
	// snippetStateStart is the initial state: the default provider has not been registered yet.
	snippetStateStart snippetState = iota
	// snippetStateAwaitProvider is the state after the default provider registration event has been
	// emitted; Next() blocks until the provider registration completes.
	snippetStateAwaitProvider
	// snippetStateDone is the terminal state after the resource event has been emitted.
	snippetStateDone
)

type snippet struct {
	snippet      *resource.Snippet
	state        snippetState
	providerDone chan *RegisterResult
}

// NewSnippetSource creates a Source that registers a single PCL resource snippet.
func NewSnippetSource(s resource.Snippet) Source {
	return &snippet{snippet: &s}
}

func (s *snippet) Close() error {
	return nil
}

func (s *snippet) Project() tokens.PackageName {
	return ""
}

func (s *snippet) Iterate(ctx context.Context, providers ProviderSource) (SourceIterator, error) {
	return s, nil
}

func (s *snippet) Cancel(ctx context.Context) error {
	return nil
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
func (s *snippet) Next() (SourceEvent, error) {
	switch s.state {
	case snippetStateStart:
		pkg := tokens.Type(s.snippet.Type).Package()
		s.providerDone = make(chan *RegisterResult)
		s.state = snippetStateAwaitProvider
		return &registerResourceEvent{
			goal: &resource.Goal{
				Type:       sdkproviders.MakeProviderType(pkg),
				Name:       "default",
				Custom:     true,
				Properties: resource.PropertyMap{},
			},
			done: s.providerDone,
		}, nil

	case snippetStateAwaitProvider:
		result := <-s.providerDone
		s.state = snippetStateDone

		ref, err := sdkproviders.NewReference(result.State.URN, result.State.ID)
		if err != nil {
			return nil, fmt.Errorf("building provider reference: %w", err)
		}

		// Bind the snippet code
		input := strings.NewReader(s.snippet.Code)
		parser := hclsyntax.NewParser()
		if err := parser.ParseFile(input, "<snippet>"); err != nil {
			return nil, fmt.Errorf("parse input: %w", err)
		}
		if parser.Diagnostics.HasErrors() {
			return nil, parser.Diagnostics
		}
		contract.Assertf(len(parser.Files) == 1, "Should be one PCL file")
		file := parser.Files[0]

		// Lookup the resource type in the provider schema and bind the snippet code to it.
		var res *schema.Resource

		attributes, resType, diags := pcl.BindResource(file, res)
		if diags.HasErrors() {
			return nil, diags
		}

		evalCtx := pclruntime.NewEvalContext("", "", "", "", "", nil, nil, nil, nil, nil)
		props, poison, diags := evalCtx.EvaluateObject(attributes, resType, res.InputProperties)
		if poison != nil {
			return nil, fmt.Errorf("snippet evaluation poisoned: %v", poison)
		}
		if diags.HasErrors() {
			return nil, diags
		}

		return &registerResourceEvent{
			goal: &resource.Goal{
				Type:       tokens.Type(s.snippet.Type),
				Name:       s.snippet.Name,
				Custom:     true,
				Provider:   ref.String(),
				Properties: props,
			},
			done: make(chan *RegisterResult, 1),
		}, nil

	default: // snippetStateDone
		return nil, nil
	}
}

// MuxSource creates a source that multiplexes the given sources, interleaving their events
// and returning nil only when all sources are exhausted. This is used to run PCL snippets
// alongside the main program source.
func NewMuxSource(sources ...Source) Source {
	// Use the first non-empty project as the mux project.
	var project tokens.PackageName
	for _, source := range sources {
		if p := source.Project(); p != "" {
			project = p
			break
		}
	}
	return &muxSource{sources: sources, project: project}
}

type muxSource struct {
	sources []Source
	project tokens.PackageName
}

func (m *muxSource) Close() error {
	var result error
	for _, source := range m.sources {
		if err := source.Close(); err != nil {
			if result == nil {
				result = err
			} else {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func (m *muxSource) Project() tokens.PackageName {
	return m.project
}

func (m *muxSource) Iterate(ctx context.Context, providers ProviderSource) (SourceIterator, error) {
	iterators := make([]SourceIterator, len(m.sources))
	for i, source := range m.sources {
		it, err := source.Iterate(ctx, providers)
		if err != nil {
			return nil, err
		}
		iterators[i] = it
	}
	return &muxSourceIterator{
		iterators: iterators,
		done:      make([]bool, len(iterators)),
	}, nil
}

type muxSourceIterator struct {
	iterators []SourceIterator
	done      []bool
	doneCount int
	current   int
}

func (m *muxSourceIterator) Cancel(ctx context.Context) error {
	var result error
	for _, it := range m.iterators {
		if err := it.Cancel(ctx); err != nil {
			if result == nil {
				result = err
			} else {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func (m *muxSourceIterator) Next() (SourceEvent, error) {
	for m.doneCount < len(m.iterators) {
		if m.current >= len(m.iterators) {
			m.current = 0
		}
		i := m.current
		m.current++
		if m.done[i] {
			continue
		}
		event, err := m.iterators[i].Next()
		if err != nil {
			return nil, err
		}
		if event == nil {
			m.done[i] = true
			m.doneCount++
			continue
		}
		return event, nil
	}
	return nil, nil
}
