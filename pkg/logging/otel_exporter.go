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
	"log/slog"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

// SlogLogExporter is a LogExporter that forwards OTLP log records
// into the slog default logger.  Property value byte attributes are
// passed through as raw []byte; the PropertySinkHandler encodes them
// for the encrypted log, and the PropertyPrimaryHandler decodes them
// to readable strings for stderr/file output.
type SlogLogExporter struct{}

func (e *SlogLogExporter) ExportLogs(_ context.Context, logs plog.Logs) error {
	for i := range logs.ResourceLogs().Len() {
		rl := logs.ResourceLogs().At(i)
		for j := range rl.ScopeLogs().Len() {
			sl := rl.ScopeLogs().At(j)
			for k := range sl.LogRecords().Len() {
				e.exportRecord(sl.LogRecords().At(k))
			}
		}
	}
	return nil
}

func (e *SlogLogExporter) Shutdown(context.Context) error { return nil }

func (e *SlogLogExporter) exportRecord(lr plog.LogRecord) {
	level := otlpSeverityToSlog(lr.SeverityNumber())
	msg := lr.Body().AsString()

	attrs := make([]any, 0, lr.Attributes().Len()*2)
	lr.Attributes().Range(func(key string, val pcommon.Value) bool {
		if val.Type() == pcommon.ValueTypeBytes {
			// Pass raw bytes — the handler wrappers will encode
			// for the sink and decode for primary.
			attrs = append(attrs, key, val.Bytes().AsRaw())
		} else {
			attrs = append(attrs, key, val.AsString())
		}
		return true
	})

	slog.Log(context.Background(), level, msg, attrs...)
}

func otlpSeverityToSlog(sev plog.SeverityNumber) slog.Level {
	switch {
	case sev >= plog.SeverityNumberError:
		return slog.LevelError
	case sev >= plog.SeverityNumberWarn:
		return slog.LevelWarn
	case sev >= plog.SeverityNumberInfo:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}
