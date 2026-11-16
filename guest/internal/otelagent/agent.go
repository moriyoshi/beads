// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package otelagent embeds the OpenTelemetry Collector in the init
// executable: the same component set the collector builder (ocb) would
// generate, and a Run function that starts it from a YAML configuration.
//
// Init runs it as a child process of itself ("/sbin/init otelcol"), not in
// PID 1, so that a panicking component cannot take init (and with it the
// kernel) down.
//
// Receivers: otlp, hostmetrics. Processors: batch, memory_limiter, resource.
// Exporters: debug, otlp, otlp_http, awscloudwatchlogs, awsemf, awsxray,
// googlecloud, azuremonitor.
package otelagent

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/debugexporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/batchprocessor"
	"go.opentelemetry.io/collector/processor/memorylimiterprocessor"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awscloudwatchlogsexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awsemfexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awsxrayexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/azuremonitorexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/googlecloudexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourceprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/hostmetricsreceiver"
)

// Version is the collector release the components come from.
const Version = "0.165.0"

// Components lists the factories built into init.
func Components() (otelcol.Factories, error) {
	f := otelcol.Factories{Telemetry: otelconftelemetry.NewFactory()}
	var err error
	if f.Receivers, err = otelcol.MakeFactoryMap[receiver.Factory](
		otlpreceiver.NewFactory(),
		hostmetricsreceiver.NewFactory(),
	); err != nil {
		return f, err
	}
	if f.Processors, err = otelcol.MakeFactoryMap[processor.Factory](
		batchprocessor.NewFactory(),
		memorylimiterprocessor.NewFactory(),
		// No resourcedetection: its detectors (Kubernetes, OpenShift, ...)
		// add ~65 MiB, and init already knows the platform, instance and
		// region and attaches them to every record it sends.
		resourceprocessor.NewFactory(),
	); err != nil {
		return f, err
	}
	if f.Exporters, err = otelcol.MakeFactoryMap[exporter.Factory](
		debugexporter.NewFactory(),
		otlpexporter.NewFactory(),
		otlphttpexporter.NewFactory(),
		awscloudwatchlogsexporter.NewFactory(),
		awsemfexporter.NewFactory(),
		awsxrayexporter.NewFactory(),
		googlecloudexporter.NewFactory(),
		azuremonitorexporter.NewFactory(),
	); err != nil {
		return f, err
	}
	return f, nil
}

// Settings returns collector settings that read the configuration from the
// given URIs: "yaml:<inline config>", "file:<path>", with ${env:VAR}
// references expanded.
func Settings(uris ...string) otelcol.CollectorSettings {
	return otelcol.CollectorSettings{
		BuildInfo: component.BuildInfo{
			Command:     "otelcol",
			Description: "OpenTelemetry Collector embedded in the beads init",
			Version:     Version,
		},
		Factories: Components,
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				URIs: uris,
				ProviderFactories: []confmap.ProviderFactory{
					yamlprovider.NewFactory(),
					fileprovider.NewFactory(),
					envprovider.NewFactory(),
				},
			},
		},
	}
}

// Run starts the collector and blocks until ctx ends or the collector stops.
func Run(ctx context.Context, uris ...string) error {
	col, err := otelcol.NewCollector(Settings(uris...))
	if err != nil {
		return err
	}
	return col.Run(ctx)
}
