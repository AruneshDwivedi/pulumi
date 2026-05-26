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

package logging

import (
	"context"
	"encoding/json"
	"log/slog"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

var marshalOpts = plugin.MarshalOptions{
	KeepSecrets:      true,
	KeepUnknowns:     true,
	KeepOutputValues: true,
}

// PropertySinkHandler wraps the encrypted log sink handler.  It
// encodes resource.PropertyMap, resource.PropertyValue, property.Map,
// and property.Value attributes into the [magic][protobuf] wire
// format so they can be decoded later by the decrypt command.
// Already-encoded bytes (from OTLP) are passed through as-is.
type PropertySinkHandler struct {
	inner slog.Handler
}

func NewPropertySinkHandler(inner slog.Handler) *PropertySinkHandler {
	return &PropertySinkHandler{inner: inner}
}

func (h *PropertySinkHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *PropertySinkHandler) Handle(ctx context.Context, r slog.Record) error {
	newRec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRec.AddAttrs(h.encodeAttr(a))
		return true
	})
	return h.inner.Handle(ctx, newRec)
}

func (h *PropertySinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	encoded := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		encoded[i] = h.encodeAttr(a)
	}
	return &PropertySinkHandler{inner: h.inner.WithAttrs(encoded)}
}

func (h *PropertySinkHandler) WithGroup(name string) slog.Handler {
	return &PropertySinkHandler{inner: h.inner.WithGroup(name)}
}

func (h *PropertySinkHandler) encodeAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() != slog.KindAny {
		return a
	}
	v := a.Value.Any()
	switch val := v.(type) {
	case resource.PropertyMap:
		if encoded := encodePropertyMap(val); encoded != nil {
			a.Value = slog.AnyValue(encoded)
		}
	case resource.PropertyValue:
		if encoded := encodePropertyValue(val); encoded != nil {
			a.Value = slog.AnyValue(encoded)
		}
	case property.Map:
		rpm := resource.ToResourcePropertyMap(val)
		if encoded := encodePropertyMap(rpm); encoded != nil {
			a.Value = slog.AnyValue(encoded)
		}
	case property.Value:
		rpv := resource.ToResourcePropertyValue(val)
		if encoded := encodePropertyValue(rpv); encoded != nil {
			a.Value = slog.AnyValue(encoded)
		}
	}
	return a
}

func encodePropertyMap(pm resource.PropertyMap) []byte {
	s, err := plugin.MarshalProperties(pm, marshalOpts)
	if err != nil {
		return nil
	}
	sv := &structpb.Value{Kind: &structpb.Value_StructValue{StructValue: s}}
	encoded, err := logging.EncodeStructValueForLog(sv)
	if err != nil {
		return nil
	}
	return encoded
}

func encodePropertyValue(pv resource.PropertyValue) []byte {
	sv, err := plugin.MarshalPropertyValue("", pv, marshalOpts)
	if err != nil || sv == nil {
		return nil
	}
	encoded, err := logging.EncodeStructValueForLog(sv)
	if err != nil {
		return nil
	}
	return encoded
}

// PropertyPrimaryHandler wraps the primary log handler (stderr/file).
// It decodes wire-format property value bytes into readable JSON
// strings for human consumption.
type PropertyPrimaryHandler struct {
	inner slog.Handler
}

func NewPropertyPrimaryHandler(inner slog.Handler) *PropertyPrimaryHandler {
	return &PropertyPrimaryHandler{inner: inner}
}

func (h *PropertyPrimaryHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *PropertyPrimaryHandler) Handle(ctx context.Context, r slog.Record) error {
	newRec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRec.AddAttrs(h.decodeAttr(a))
		return true
	})
	return h.inner.Handle(ctx, newRec)
}

func (h *PropertyPrimaryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	decoded := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		decoded[i] = h.decodeAttr(a)
	}
	return &PropertyPrimaryHandler{inner: h.inner.WithAttrs(decoded)}
}

func (h *PropertyPrimaryHandler) WithGroup(name string) slog.Handler {
	return &PropertyPrimaryHandler{inner: h.inner.WithGroup(name)}
}

func (h *PropertyPrimaryHandler) decodeAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() != slog.KindAny {
		return a
	}
	switch val := a.Value.Any().(type) {
	case []byte:
		sv, err := logging.DecodeStructValueFromLog(val)
		if err != nil {
			return a
		}
		if s := sv.GetStructValue(); s != nil {
			pm, err := plugin.UnmarshalProperties(s, marshalOpts)
			if err == nil {
				b, _ := json.Marshal(pm.Mappable())
				a.Value = slog.StringValue(string(b))
				return a
			}
		}
		b, _ := json.Marshal(sv.AsInterface())
		a.Value = slog.StringValue(string(b))
	case resource.PropertyMap:
		b, _ := json.Marshal(val.Mappable())
		a.Value = slog.StringValue(string(b))
	case resource.PropertyValue:
		b, _ := json.Marshal(val.Mappable())
		a.Value = slog.StringValue(string(b))
	case property.Map:
		rpm := resource.ToResourcePropertyMap(val)
		b, _ := json.Marshal(rpm.Mappable())
		a.Value = slog.StringValue(string(b))
	case property.Value:
		rpv := resource.ToResourcePropertyValue(val)
		b, _ := json.Marshal(rpv.Mappable())
		a.Value = slog.StringValue(string(b))
	}
	return a
}
