// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package otlpsink is a minimal OTLP/HTTP receiver for tests: it accepts
// protobuf or JSON export requests (optionally gzip-compressed) on
// /v1/logs, /v1/traces and /v1/metrics, and keeps what arrives: log
// records, span names and metric names.
package otlpsink

import (
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Record is a received log record, flattened.
type Record struct {
	Body     string
	Scope    string
	Resource map[string]string
	Attrs    map[string]string
}

// Sink collects records.
type Sink struct {
	mu      sync.Mutex
	records []Record
	spans   []string
	metrics []string
	srv     *http.Server
	l       net.Listener
}

// Start listens on addr (e.g. "127.0.0.1:0").
func Start(addr string) (*Sink, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Sink{l: l}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/logs", s.handle)
	mux.HandleFunc("/v1/traces", s.handleTraces)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(l)
	return s, nil
}

// Port is the port the sink listens on.
func (s *Sink) Port() int { return s.l.Addr().(*net.TCPAddr).Port }

// Close stops the server.
func (s *Sink) Close() error { return s.srv.Close() }

// Records returns what has arrived so far.
func (s *Sink) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// decode reads an OTLP export request body into req.
func decode(w http.ResponseWriter, r *http.Request, req proto.Message) bool {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		body = zr
	}
	b, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err == nil {
		if isJSON(r) {
			err = protojson.Unmarshal(b, req)
		} else {
			err = proto.Unmarshal(b, req)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func isJSON(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

func reply(w http.ResponseWriter, r *http.Request, resp proto.Message) {
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	var b []byte
	if isJSON(r) {
		b, _ = protojson.Marshal(resp)
	} else {
		b, _ = proto.Marshal(resp)
	}
	w.Write(b)
}

func (s *Sink) handleTraces(w http.ResponseWriter, r *http.Request) {
	var req coltrace.ExportTraceServiceRequest
	if !decode(w, r, &req) {
		return
	}
	var names []string
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				names = append(names, sp.GetName())
			}
		}
	}
	s.mu.Lock()
	s.spans = append(s.spans, names...)
	s.mu.Unlock()
	reply(w, r, &coltrace.ExportTraceServiceResponse{})
}

func (s *Sink) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var req colmetrics.ExportMetricsServiceRequest
	if !decode(w, r, &req) {
		return
	}
	var names []string
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				names = append(names, m.GetName())
			}
		}
	}
	s.mu.Lock()
	s.metrics = append(s.metrics, names...)
	s.mu.Unlock()
	reply(w, r, &colmetrics.ExportMetricsServiceResponse{})
}

func (s *Sink) handle(w http.ResponseWriter, r *http.Request) {
	var req collogs.ExportLogsServiceRequest
	if !decode(w, r, &req) {
		return
	}
	var got []Record
	for _, rl := range req.GetResourceLogs() {
		res := map[string]string{}
		for _, kv := range rl.GetResource().GetAttributes() {
			res[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := map[string]string{}
				for _, kv := range lr.GetAttributes() {
					attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
				}
				got = append(got, Record{
					Body:     lr.GetBody().GetStringValue(),
					Scope:    sl.GetScope().GetName(),
					Resource: res,
					Attrs:    attrs,
				})
			}
		}
	}
	s.mu.Lock()
	s.records = append(s.records, got...)
	s.mu.Unlock()
	reply(w, r, &collogs.ExportLogsServiceResponse{})
}

// Spans returns the names of the spans received so far.
func (s *Sink) Spans() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.spans...)
}

// Metrics returns the names of the metrics received so far (with repeats).
func (s *Sink) Metrics() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.metrics...)
}

// MissingNames returns the wanted names absent from got.
func MissingNames(got, want []string) []string {
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// Missing returns the wanted substrings that no record body contains.
func (s *Sink) Missing(want []string) []string {
	recs := s.Records()
	var missing []string
	for _, w := range want {
		found := false
		for _, r := range recs {
			if strings.Contains(r.Body, w) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

// Summary describes what arrived, for test logs.
func (s *Sink) Summary() string {
	recs := s.Records()
	scopes := map[string]int{}
	for _, r := range recs {
		scopes[r.Scope]++
	}
	return fmt.Sprintf("%d log records %v, %d spans, %d metric points", len(recs), scopes, len(s.Spans()), len(s.Metrics()))
}
