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

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

type snippet struct {
	snippet *resource.Snippet
	project tokens.PackageName
}

// Snippet represents a snippet of PCL that should be associated with a stack. The engine reruns these in deployments.
func NewSnippet(project tokens.PackageName, args *resource.Snippet) Source {
	return &snippet{project: project, snippet: args}
}

func (s *snippet) Close() error {
	return nil
}

func (s *snippet) Project() tokens.PackageName {
	return s.project
}

func (s *snippet) Iterate(ctx context.Context, providers ProviderSource) (SourceIterator, error) {
	return s, nil
}

func (s *snippet) Cancel(ctx context.Context) error {
	return nil
}

func (s *snippet) Next() (SourceEvent, error) {
	goal := &resource.Goal{
		Type: tokens.Type(s.snippet.Type),
		Name: s.snippet.Name,
	}
	return &registerResourceEvent{
		goal: goal,
	}, nil
}

// NewMuxSource creates a new source that muxes together the given sources. The resulting source will execute each of
// the given sources in order, and aggregate their diagnostics and events. This is used to run PCL snippets in the
// engine.
func NewMuxSource(sources ...Source) Source {
	// Assert that all the sources are for the same project
	var project tokens.PackageName
	for _, source := range sources {
		if project == "" {
			project = source.Project()
		} else if source.Project() != project {
			panic("all sources must be for the same project")
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

	return &muxSourceIterator{iterators: iterators}, nil
}

type muxSourceIterator struct {
	iterators []SourceIterator
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
	if m.current >= len(m.iterators) {
		m.current = 0
	}

	i := m.current
	m.current++

	return m.iterators[i].Next()
}
