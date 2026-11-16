// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package logship

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/moriyoshi/beads/guest/internal/otelconfig"
	"github.com/moriyoshi/beads/internal/otlpsink"
)

type memExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memExporter) Export(_ context.Context, rs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rs {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memExporter) Shutdown(context.Context) error   { return nil }
func (m *memExporter) ForceFlush(context.Context) error { return nil }

func TestCaptureTeesAndSplitsLines(t *testing.T) {
	exp := &memExporter{}
	in := otelconfig.Instance{Platform: "ec2", InstanceID: "i-1", Region: "ap-northeast-1"}
	s := NewWithExporter(exp, Resource("hello", in))
	var console bytes.Buffer
	c := &Capture{Console: &console, Shipper: s, Scope: "app", Stream: "stderr", Severity: otellog.SeverityInfo}
	c.Write([]byte("first line\nsecond "))
	c.Write([]byte("line\r\n\npartial"))
	c.Flush()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if console.String() != "first line\nsecond line\r\n\npartial" {
		t.Errorf("console got %q", console.String())
	}
	var bodies []string
	for _, r := range exp.records {
		bodies = append(bodies, r.Body().AsString())
	}
	if got := strings.Join(bodies, "|"); got != "first line|second line|partial" {
		t.Errorf("records %q", got)
	}
	r := exp.records[0]
	var stream string
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "log.iostream" {
			stream = kv.Value.AsString()
		}
		return true
	})
	if stream != "stderr" || r.SeverityText() != "INFO" || r.InstrumentationScope().Name != "app" {
		t.Errorf("record stream=%q severity=%q scope=%q", stream, r.SeverityText(), r.InstrumentationScope().Name)
	}
	attrs := map[string]string{}
	for _, kv := range r.Resource().Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "hello" || attrs["cloud.provider"] != "aws" || attrs["host.id"] != "i-1" || attrs["cloud.region"] != "ap-northeast-1" {
		t.Errorf("resource %v", attrs)
	}
}

func TestShipsOverOTLP(t *testing.T) {
	sink, err := otlpsink.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	s, err := New(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", sink.Port()),
		Resource("hello", otelconfig.Instance{Platform: "qemu"}))
	if err != nil {
		t.Fatal(err)
	}
	c := &Capture{Shipper: s, Scope: "app", Stream: "stdout", Severity: otellog.SeverityInfo}
	c.Write([]byte("hello over otlp\n"))
	s.Emit("nbinit", otellog.SeverityInfo, "init line")
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := sink.Missing([]string{"hello over otlp", "init line"}); len(m) != 0 {
		t.Fatalf("missing %v; got %s", m, sink.Summary())
	}
	for _, r := range sink.Records() {
		if r.Resource["service.name"] != "hello" || r.Resource["os.type"] != "netbsd" {
			t.Errorf("resource %v", r.Resource)
		}
		if r.Body == "hello over otlp" && (r.Scope != "app" || r.Attrs["log.iostream"] != "stdout") {
			t.Errorf("record %+v", r)
		}
	}
}

func TestNilShipper(t *testing.T) {
	var s *Shipper
	s.Emit("app", otellog.SeverityInfo, "dropped")
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	c := &Capture{Console: &console, Shipper: s}
	c.Write([]byte("still printed\n"))
	if console.String() != "still printed\n" {
		t.Errorf("console %q", console.String())
	}
}
