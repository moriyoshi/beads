// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command hello is the example appliance program: an HTTP server that
// reports where it runs, greeting with $HELLO_GREETING (default "hello"),
// shuts down gracefully on SIGTERM, and, when init
// points it at a collector through the standard OTEL_* variables, records
// a span per request and a request counter with the OpenTelemetry SDK.
// Given $APPLIANCE_LOCAL_ADDRS, it checks that it answers on those
// addresses and that its own connections do not leave from them.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// setupTelemetry installs trace and meter providers that export over OTLP
// to OTEL_EXPORTER_OTLP_ENDPOINT, with the resource from OTEL_SERVICE_NAME
// and OTEL_RESOURCE_ATTRIBUTES. The returned function flushes and stops them.
func setupTelemetry(ctx context.Context) (func(context.Context), error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) {}, nil
	}
	res, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil {
		return nil, err
	}
	te, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	me, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(15*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return func(ctx context.Context) {
		tp.Shutdown(ctx)
		mp.Shutdown(ctx) // exports the final readings
	}, nil
}

// checkLocalAddrs GETs / on each address init added to lo0, as listed
// in $APPLIANCE_LOCAL_ADDRS, and checks that the kernel does not pick one
// of them as the source of an outbound connection; the boot tests look for
// the results in the log.
func checkLocalAddrs(port int) {
	v := os.Getenv("APPLIANCE_LOCAL_ADDRS")
	if v == "" {
		return
	}
	aliases := map[string]bool{}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, cidr := range strings.Split(v, ",") {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Printf("hello: APPLIANCE_LOCAL_ADDRS: %v", err)
			continue
		}
		aliases[ip.String()] = true
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			log.Printf("hello: no answer on %s: %v", addr, err)
			continue
		}
		resp.Body.Close()
		log.Printf("hello: answered on %s", addr)
	}
	// Connecting a UDP socket sends nothing, but makes the kernel choose the
	// source address as it would for any outbound connection.
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		log.Printf("hello: outbound source: %v", err)
		return
	}
	src := c.LocalAddr().(*net.UDPAddr).IP
	c.Close()
	if aliases[src.String()] {
		log.Printf("hello: outbound source is one of APPLIANCE_LOCAL_ADDRS (%s)", src)
	} else {
		log.Printf("hello: outbound source is not one of APPLIANCE_LOCAL_ADDRS (%s)", src)
	}
}

func main() {
	listen := flag.String("listen", ":8080", "address to serve on")
	flag.Parse()
	host, _ := os.Hostname()
	greeting := os.Getenv("HELLO_GREETING")
	if greeting == "" {
		greeting = "hello"
	}

	shutdownTelemetry, err := setupTelemetry(context.Background())
	if err != nil {
		log.Printf("hello: telemetry: %v", err)
		shutdownTelemetry = func(context.Context) {}
	}
	tracer := otel.Tracer("hello")
	requests, _ := otel.Meter("hello").Int64Counter("hello.requests", metric.WithDescription("HTTP requests served"))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A server span: it handles an incoming request. (X-Ray also names
		// the segment of a server span after service.name.)
		_, span := tracer.Start(r.Context(), "hello.request", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.path", r.URL.Path))
		requests.Add(r.Context(), 1, metric.WithAttributes(attribute.String("url.path", r.URL.Path)))
		fmt.Fprintf(w, "%s from %s/%s (host %q, pid %d)\n", greeting, runtime.GOOS, runtime.GOARCH, host, os.Getpid())
	})
	srv := &http.Server{Addr: *listen, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("hello: shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	// On NetBSD the system pool is read from /etc/openssl/certs, so this
	// shows which CA bundle the image carries.
	if pool, err := x509.SystemCertPool(); err == nil {
		log.Printf("hello: %d trusted CA certificates", len(pool.Subjects())) //nolint:staticcheck // fine for file-based pools
	} else {
		log.Printf("hello: no system CA pool: %v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("hello: listening on %s", *listen)
	go checkLocalAddrs(ln.Addr().(*net.TCPAddr).Port)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownTelemetry(sctx) // flush spans and the final metric readings
	cancel()
	log.Printf("hello: stopped")
}
