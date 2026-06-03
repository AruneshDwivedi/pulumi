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

package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	escEncoding "github.com/pulumi/esc/syntax/encoding"
	"gopkg.in/yaml.v3"

	"github.com/pulumi/pulumi/pkg/v3/backend"
	cmdStack "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/stack"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

// configEditor is a write-focused abstraction over a stack's configuration store. Mutations are
// buffered and persisted on Save. Callers pass plaintext for secrets (a config.Value with
// Secure()=true whose raw value is the plaintext); the editor is responsible for encrypting or
// otherwise protecting the secret according to where the config lives.
type configEditor interface {
	// If path is true, key's name is a property path within a map or list.
	Set(ctx context.Context, key config.Key, value config.Value, path bool) error
	// If path is true, key's name is a property path. Removing an absent key is a no-op.
	Remove(ctx context.Context, key config.Key, path bool) error
	Save(ctx context.Context) error
}

// newConfigEditor returns a configEditor for the stack's configuration store. encrypter is used by
// the local editor to encrypt secret values before they are written to the stack file; it is unused
// for stores that protect secrets themselves (the ESC-backed editor wraps secrets as fn::secret and
// relies on the service to encrypt them).
func newConfigEditor(
	ctx context.Context, stack backend.Stack, ps *workspace.ProjectStack, encrypter config.Encrypter, configFile string,
) (configEditor, error) {
	if configStoreIsRemote(stack, configFile) {
		return newESCConfigEditor(ctx, stack)
	}
	return &localConfigEditor{stack: stack, ps: ps, encrypter: encrypter, configFile: configFile}, nil
}

// configStoreIsRemote reports whether the stack's configuration is effectively stored remotely. An
// explicit --config-file selects a local file regardless of the stack's linked location, mirroring
// the precedence in cmdStack.LoadProjectStack/SaveProjectStack.
func configStoreIsRemote(stack backend.Stack, configFile string) bool {
	return configFile == "" && stack.ConfigLocation().IsRemote
}

// ConfigStoreIsRemote reports whether the stack's configuration is effectively stored remotely,
// honoring an explicit --config-file.
func ConfigStoreIsRemote(stack backend.Stack, configFile string) bool {
	return configStoreIsRemote(stack, configFile)
}

// SaveRemoteConfigValues writes c to the ESC environment backing a remote-config stack via the
// editor's read-modify-write, so unrelated keys, imports, and comments are preserved and a concurrent
// edit surfaces as a retryable etag conflict. Existing keys present in c are overwritten. Secret
// values (a config.Value with Secure()=true carrying plaintext) are wrapped as fn::secret and
// encrypted server-side. The stack must already be linked to an environment.
func SaveRemoteConfigValues(ctx context.Context, stack backend.Stack, c config.Map) error {
	editor, err := newESCConfigEditor(ctx, stack)
	if err != nil {
		return err
	}
	keys := make(config.KeyArray, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Sort(keys)
	for _, k := range keys {
		if err := editor.Set(ctx, k, c[k], false /*path*/); err != nil {
			return err
		}
	}
	return editor.Save(ctx)
}

// checkRemoteProjectStack guards mutation commands against a nil ProjectStack for remote stacks.
// cloudStack.LoadRemoteConfig returns (nil, nil) when the service-side stack config is absent, and
// LoadProjectStack passes that through; dereferencing it would panic.
func checkRemoteProjectStack(stack backend.Stack, ps *workspace.ProjectStack) error {
	if ps == nil && stack.ConfigLocation().IsRemote {
		return errors.New("the stack has no remote configuration; run `pulumi config env init` to create one")
	}
	return nil
}

type localConfigEditor struct {
	stack      backend.Stack
	ps         *workspace.ProjectStack
	encrypter  config.Encrypter
	configFile string
}

func (e *localConfigEditor) Set(ctx context.Context, key config.Key, value config.Value, path bool) error {
	// Secure object values already carry per-leaf ciphertext the caller produced; encrypting the
	// whole serialized object as one blob would corrupt it, so only scalar secrets are encrypted here.
	if value.Secure() && !value.Object() {
		plaintext, err := value.Value(config.NopDecrypter)
		if err != nil {
			return err
		}
		encrypted, err := e.encrypter.EncryptValue(ctx, plaintext)
		if err != nil {
			return err
		}
		value = config.NewSecureValue(encrypted)
	}
	return e.ps.Config.Set(key, value, path)
}

func (e *localConfigEditor) Remove(_ context.Context, key config.Key, path bool) error {
	return e.ps.Config.Remove(key, path)
}

func (e *localConfigEditor) Save(ctx context.Context) error {
	return cmdStack.SaveProjectStack(ctx, e.stack, e.ps, e.configFile)
}

// escConfigEditor edits configuration stored remotely in an ESC environment that backs the stack.
// It buffers edits against an in-memory YAML document (read on construction) and uploads the result
// on Save using read-modify-write semantics keyed by the environment's etag. Secrets are wrapped as
// fn::secret nodes and encrypted server-side; this editor never encrypts or logs secret values.
type escConfigEditor struct {
	envBackend backend.EnvironmentsBackend
	orgName    string
	envProject string
	envName    string
	doc        yaml.Node
	etag       string
}

func newESCConfigEditor(ctx context.Context, stack backend.Stack) (*escConfigEditor, error) {
	envBackend, ok := stack.Backend().(backend.EnvironmentsBackend)
	if !ok {
		return nil, fmt.Errorf("backend %v does not support environments", stack.Backend().Name())
	}

	orgNamer, ok := stack.(interface{ OrgName() string })
	if !ok {
		return nil, errors.New("internal error: stack does not provide an organization name")
	}
	orgName := orgNamer.OrgName()

	ref := stack.ConfigLocation().EscEnv
	if ref == nil {
		return nil, errors.New("stack is configured for remote config but has no linked environment")
	}
	// The linked env ref has the form "<project>/<name>" and may carry an "@version" suffix.
	envRef, _, _ := strings.Cut(*ref, "@")
	envProject, envName, found := strings.Cut(envRef, "/")
	if !found {
		return nil, fmt.Errorf("invalid environment reference %q: expected <project>/<name>", *ref)
	}

	def, etag, _, err := envBackend.GetEnvironment(ctx, orgName, envProject, envName, "", false)
	if err != nil {
		return nil, fmt.Errorf("getting environment definition: %w", err)
	}

	var doc yaml.Node
	if len(def) != 0 {
		if err := yaml.Unmarshal(def, &doc); err != nil {
			return nil, fmt.Errorf("unmarshaling environment definition: %w", err)
		}
	}
	if doc.Kind != yaml.DocumentNode {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{}}}
	}

	return &escConfigEditor{
		envBackend: envBackend,
		orgName:    orgName,
		envProject: envProject,
		envName:    envName,
		doc:        doc,
		etag:       etag,
	}, nil
}

func (e *escConfigEditor) Set(ctx context.Context, key config.Key, value config.Value, path bool) error {
	valueNode, err := configValueToYAMLNode(ctx, key, value)
	if err != nil {
		return err
	}

	configPath, err := pulumiConfigPath(key, path)
	if err != nil {
		return err
	}

	valuesNode, err := e.ensureValuesNode()
	if err != nil {
		return err
	}

	if _, err := (escEncoding.YAMLSyntax{Node: valuesNode}).Set(nil, configPath, valueNode); err != nil {
		return err
	}
	return nil
}

func (e *escConfigEditor) Remove(_ context.Context, key config.Key, path bool) error {
	configPath, err := pulumiConfigPath(key, path)
	if err != nil {
		return err
	}

	valuesNode, ok := escEncoding.YAMLSyntax{Node: &e.doc}.Get(resource.PropertyPath{"values"})
	if !ok {
		return nil
	}
	// Delete assumes every intermediate node on the path exists; if the full path is absent, treat
	// the removal as a no-op rather than traversing a missing parent.
	if _, ok := (escEncoding.YAMLSyntax{Node: valuesNode}).Get(configPath); !ok {
		return nil
	}
	return escEncoding.YAMLSyntax{Node: valuesNode}.Delete(nil, configPath)
}

func (e *escConfigEditor) Save(ctx context.Context) error {
	newYAML, err := yaml.Marshal(e.doc.Content[0])
	if err != nil {
		return fmt.Errorf("marshaling definition: %w", err)
	}

	diags, err := e.envBackend.UpdateEnvironmentWithProject(ctx, e.orgName, e.envProject, e.envName, newYAML, e.etag)
	if err != nil {
		if errors.Is(err, backend.ErrConfigConflict) {
			return fmt.Errorf("the stack's configuration was modified concurrently; please retry: %w", err)
		}
		return err
	}
	if len(diags) != 0 {
		return fmt.Errorf("updating environment: %w", diags)
	}
	return nil
}

// Imports returns the names of the entries in the environment's top-level `imports` sequence, or an
// empty slice if absent. Both plain scalar entries and structured entries (e.g. {env: {merge: false}})
// are reported by name. Unlike workspace.Environment.Imports it does not append the synthetic "yaml"
// marker for inline values: that marker is a local-Environment artifact and is meaningless for the
// backing env's real imports list.
func (e *escConfigEditor) Imports() []string {
	seq, ok := escEncoding.YAMLSyntax{Node: &e.doc}.Get(resource.PropertyPath{"imports"})
	if !ok || seq.Kind != yaml.SequenceNode {
		return []string{}
	}
	imports := make([]string, 0, len(seq.Content))
	for _, n := range seq.Content {
		if name, ok := importEntryName(n); ok {
			imports = append(imports, name)
		}
	}
	return imports
}

// importEntryName returns the environment name of an `imports` entry: the scalar value for a plain
// entry, or the single key for a structured entry like {env: {merge: false}}.
func importEntryName(n *yaml.Node) (string, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value, true
	case yaml.MappingNode:
		// A well-formed structured import is a single-key mapping ({env: {merge: ...}}), so Content has
		// exactly the key and its value. Reject multi-key mappings rather than matching the first key.
		if len(n.Content) == 2 && n.Content[0].Kind == yaml.ScalarNode {
			return n.Content[0].Value, true
		}
		return "", false
	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
		return "", false
	default:
		return "", false
	}
}

// AddImports appends each env to the top-level `imports` sequence, creating it if absent. It mirrors
// workspace.Environment.Append: entries are appended in order and not de-duplicated.
func (e *escConfigEditor) AddImports(envs ...string) error {
	seq, err := e.ensureImportsNode()
	if err != nil {
		return err
	}
	for _, env := range envs {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: env})
	}
	return nil
}

// RemoveImport removes the last entry matching env (by name, so structured entries like
// {env: {merge: false}} match too) from the top-level `imports` sequence, mirroring
// workspace.Environment.Remove. Removing an absent entry is a no-op. The (possibly empty) sequence
// node is left in place rather than deleting the `imports` key, so a head comment attached to that
// key survives removal of the last import.
func (e *escConfigEditor) RemoveImport(env string) error {
	seq, ok := escEncoding.YAMLSyntax{Node: &e.doc}.Get(resource.PropertyPath{"imports"})
	if !ok || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for i := len(seq.Content) - 1; i >= 0; i-- {
		if name, ok := importEntryName(seq.Content[i]); ok && name == env {
			seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			return nil
		}
	}
	return nil
}

func (e *escConfigEditor) ensureImportsNode() (*yaml.Node, error) {
	seq, ok := escEncoding.YAMLSyntax{Node: &e.doc}.Get(resource.PropertyPath{"imports"})
	if ok {
		// An existing `imports` that is not a sequence is a malformed env; overwriting it would
		// silently discard it, so refuse rather than clobber.
		if seq.Kind != yaml.SequenceNode {
			return nil, errors.New("environment's `imports` is not a sequence")
		}
		return seq, nil
	}
	seq, err := escEncoding.YAMLSyntax{Node: &e.doc}.Set(
		nil, resource.PropertyPath{"imports"}, yaml.Node{Kind: yaml.SequenceNode})
	if err != nil {
		return nil, fmt.Errorf("internal error: %w", err)
	}
	return seq, nil
}

func (e *escConfigEditor) ensureValuesNode() (*yaml.Node, error) {
	valuesNode, ok := escEncoding.YAMLSyntax{Node: &e.doc}.Get(resource.PropertyPath{"values"})
	if ok {
		return valuesNode, nil
	}
	valuesNode, err := escEncoding.YAMLSyntax{Node: &e.doc}.Set(
		nil, resource.PropertyPath{"values"}, yaml.Node{Kind: yaml.MappingNode})
	if err != nil {
		return nil, fmt.Errorf("internal error: %w", err)
	}
	return valuesNode, nil
}

// pulumiConfigPath builds the property path to a config key within an environment's `values` node.
// The returned path is rooted at `pulumiConfig`. For non-path keys it is {"pulumiConfig", "<ns:name>"}.
// For path keys, the key's name is itself a property path within the top-level config value; the
// top-level segment is namespaced ("<ns>:<segment>") and remaining segments follow. Array-index
// segments are not supported because ESC config keys are flat dotted strings.
func pulumiConfigPath(key config.Key, path bool) (resource.PropertyPath, error) {
	if !path {
		return resource.PropertyPath{"pulumiConfig", key.String()}, nil
	}

	nameSegments, err := resource.ParsePropertyPath(key.Name())
	if err != nil {
		return nil, fmt.Errorf("invalid configuration key path: %w", err)
	}
	if len(nameSegments) == 0 {
		return nil, errors.New("configuration key path is empty")
	}
	for _, seg := range nameSegments {
		if _, isInt := seg.(int); isInt {
			return nil, errors.New("array index paths are not supported for remote config; " +
				"use `pulumi config edit` to modify array values directly")
		}
	}

	first := nameSegments[0].(string)

	result := make(resource.PropertyPath, 0, len(nameSegments)+1)
	result = append(result, "pulumiConfig", key.Namespace()+":"+first)
	result = append(result, nameSegments[1:]...)
	return result, nil
}

// configValueToYAMLNode converts a config.Value (carrying plaintext for secrets) into a YAML node
// with native types preserved and secrets wrapped as {fn::secret: <plaintext>}.
func configValueToYAMLNode(ctx context.Context, key config.Key, value config.Value) (yaml.Node, error) {
	pm, err := config.Map{key: value}.AsDecryptedPropertyMap(ctx, config.NopDecrypter)
	if err != nil {
		return yaml.Node{}, err
	}
	pv, ok := pm.GetOk(key.String())
	if !ok {
		return yaml.Node{}, fmt.Errorf("internal error: config value for %q not found after conversion", key.String())
	}

	rendered := renderConfigValueForESC(pv)

	// A whole-object secret (e.g. `config set-all --json` with objectValue and secret:true) loses its
	// top-level secret flag in AsDecryptedPropertyMap: object secrets are carried as nested markers,
	// not a top-level flag, so the rendered object is plain. Wrap it as fn::secret here so the service
	// encrypts the whole object.
	if value.Secure() && value.Object() {
		rendered = map[string]any{"fn::secret": rendered}
	}

	b, err := yaml.Marshal(rendered)
	if err != nil {
		return yaml.Node{}, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return yaml.Node{}, err
	}
	if len(node.Content) == 0 {
		return yaml.Node{}, errors.New("internal error: empty value node")
	}
	return *node.Content[0], nil
}

// renderConfigValueForESC recursively converts a property.Value into native Go values suitable for
// an ESC environment definition, preserving scalar types (bool/number/string) and wrapping secrets
// as {fn::secret: <inner>}. This is the same conversion `pulumi config env init` performs.
func renderConfigValueForESC(v property.Value) any {
	switch {
	case v.Secret():
		return map[string]any{
			"fn::secret": renderConfigValueForESC(v.WithSecret(false)),
		}
	case v.IsBool():
		return v.AsBool()
	case v.IsNumber():
		return v.AsNumber()
	case v.IsString():
		return v.AsString()
	case v.IsArray():
		arrV := v.AsArray()
		rendered := make([]any, arrV.Len())
		for i, v := range arrV.All {
			rendered[i] = renderConfigValueForESC(v)
		}
		return rendered
	case v.IsMap():
		objV := v.AsMap()
		rendered := make(map[string]any, objV.Len())
		for k, v := range objV.All {
			rendered[k] = renderConfigValueForESC(v)
		}
		return rendered
	default:
		return nil
	}
}
