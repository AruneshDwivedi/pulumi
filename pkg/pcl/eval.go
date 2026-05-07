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

package pcl

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/zclconf/go-cty/cty"
)

// EvalBody parses a PCL object expression and evaluates it into a resource.PropertyMap.
// The expression must be an HCL object literal, e.g. "{ key = value }".
// Only literal expressions are supported; cross-resource references and config variables
// will produce evaluation errors.
func EvalBody(code string) (resource.PropertyMap, hcl.Diagnostics) {
	expr, diags := hclsyntax.ParseExpression([]byte(code), "<snippet>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, diags
	}

	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return nil, diags
	}

	result, err := ctyObjectToPropertyMap(val)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  err.Error(),
		}}
	}
	return result, nil
}

func ctyObjectToPropertyMap(val cty.Value) (resource.PropertyMap, error) {
	if val.IsNull() {
		return resource.PropertyMap{}, nil
	}
	t := val.Type()
	if !t.IsObjectType() && !t.IsMapType() {
		return nil, fmt.Errorf("expected object type for resource body, got %s", t.FriendlyName())
	}
	result := resource.PropertyMap{}
	for it := val.ElementIterator(); it.Next(); {
		k, v := it.Element()
		pv, err := CtyToPropertyValue(v)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", k.AsString(), err)
		}
		result[resource.PropertyKey(k.AsString())] = pv
	}
	return result, nil
}

// CtyToPropertyValue converts a cty.Value to a resource.PropertyValue.
// Supports: null, unknown (as computed), bool, number, string, list, tuple, map, object.
func CtyToPropertyValue(val cty.Value) (resource.PropertyValue, error) {
	if !val.IsKnown() {
		return resource.NewProperty(resource.Computed{Element: resource.NewProperty("")}), nil
	}
	if val.IsNull() {
		return resource.NewNullProperty(), nil
	}

	switch val.Type() {
	case cty.String:
		return resource.NewProperty(val.AsString()), nil
	case cty.Bool:
		return resource.NewProperty(val.True()), nil
	case cty.Number:
		f, _ := val.AsBigFloat().Float64()
		return resource.NewProperty(f), nil
	}

	switch {
	case val.Type().IsListType() || val.Type().IsTupleType():
		var elements []resource.PropertyValue
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			pv, err := CtyToPropertyValue(v)
			if err != nil {
				return resource.PropertyValue{}, err
			}
			elements = append(elements, pv)
		}
		return resource.NewProperty(elements), nil
	case val.Type().IsMapType() || val.Type().IsObjectType():
		obj := resource.PropertyMap{}
		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			pv, err := CtyToPropertyValue(v)
			if err != nil {
				return resource.PropertyValue{}, err
			}
			obj[resource.PropertyKey(k.AsString())] = pv
		}
		return resource.NewProperty(obj), nil
	}

	return resource.PropertyValue{}, fmt.Errorf("unsupported cty type: %s", val.Type().FriendlyName())
}
