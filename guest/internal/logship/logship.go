// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package logship turns the appliance program's output, and init's own
// messages, into OpenTelemetry log records and sends them over OTLP/HTTP to
// the collector embedded in init, which forwards them to the platform's log
// service.
//
// Every line still reaches the serial console unchanged; shipping is in
// addition to it, through the OpenTelemetry SDK's batching log pipeline.
package logship

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/moriyoshi/beads/guest/internal/otelconfig"
)

// Resource describes the appliance with OpenTelemetry semantic-convention
// attributes. Init attaches it to every record, standing in for the
// collector's resourcedetection processor, which is not built in.
func Resource(service string, in otelconfig.Instance) *resource.Resource {
	var attrs []attribute.KeyValue
	for _, kv := range otelconfig.ResourceAttrs(&otelconfig.Config{ServiceName: service}, in) {
		attrs = append(attrs, attribute.String(kv[0], kv[1]))
	}
	return resource.NewSchemaless(attrs...)
}

// Shipper owns the log pipeline. A nil *Shipper accepts and drops records,
// so callers need no special case when shipping is off.
type Shipper struct {
	provider *sdklog.LoggerProvider
	mu       sync.Mutex
	loggers  map[string]otellog.Logger
}

// New sends records to an OTLP/HTTP receiver at endpoint (e.g.
// "http://127.0.0.1:4318").
func New(ctx context.Context, endpoint string, res *resource.Resource) (*Shipper, error) {
	exp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(endpoint+"/v1/logs"))
	if err != nil {
		return nil, err
	}
	return NewWithExporter(exp, res), nil
}

// NewWithExporter builds the pipeline around any exporter.
func NewWithExporter(exp sdklog.Exporter, res *resource.Resource) *Shipper {
	return &Shipper{
		loggers: map[string]otellog.Logger{},
		provider: sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp,
				sdklog.WithExportInterval(2*time.Second),
				sdklog.WithExportTimeout(15*time.Second),
			)),
		),
	}
}

func (s *Shipper) logger(scope string) otellog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.loggers[scope]
	if !ok {
		l = s.provider.Logger(scope)
		s.loggers[scope] = l
	}
	return l
}

// Emit records one line. scope names the source ("app", "nbinit").
func (s *Shipper) Emit(scope string, sev otellog.Severity, line string, attrs ...attribute.KeyValue) {
	if s == nil {
		return
	}
	var r otellog.Record
	now := time.Now()
	r.SetTimestamp(now)
	r.SetObservedTimestamp(now)
	r.SetSeverity(sev)
	r.SetSeverityText(severityText(sev))
	r.SetBody(attribute.StringValue(line))
	r.AddAttributes(attrs...)
	s.logger(scope).Emit(context.Background(), r)
}

func severityText(s otellog.Severity) string {
	switch {
	case s >= otellog.SeverityError:
		return "ERROR"
	case s >= otellog.SeverityWarn:
		return "WARN"
	case s >= otellog.SeverityInfo:
		return "INFO"
	case s > 0:
		return "DEBUG"
	}
	return ""
}

// Shutdown flushes buffered records and stops the pipeline.
func (s *Shipper) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.provider.Shutdown(ctx)
}

// Capture is an io.Writer for one output stream of the program: it copies
// everything to the console and emits each complete line as a record.
type Capture struct {
	Console  io.Writer
	Shipper  *Shipper
	Scope    string
	Stream   string // "stdout" or "stderr"
	Severity otellog.Severity

	mu  sync.Mutex
	buf []byte
}

// maxLine bounds a record; longer lines are split.
const maxLine = 16 << 10

func (c *Capture) Write(p []byte) (int, error) {
	if c.Console != nil {
		c.Console.Write(p)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			if len(c.buf) >= maxLine {
				c.emit(c.buf[:maxLine])
				c.buf = c.buf[maxLine:]
				continue
			}
			return len(p), nil
		}
		c.emit(c.buf[:i])
		c.buf = c.buf[i+1:]
	}
}

// Flush emits a final line that lacked a newline.
func (c *Capture) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) > 0 {
		c.emit(c.buf)
		c.buf = nil
	}
}

func (c *Capture) emit(line []byte) {
	s := strings.TrimRight(string(line), "\r")
	if s == "" {
		return
	}
	c.Shipper.Emit(c.Scope, c.Severity, s, attribute.String("log.iostream", c.Stream))
}
