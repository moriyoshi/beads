// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package otelagent

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/otelcol"

	"github.com/moriyoshi/beads/guest/internal/otelconfig"
)

// Every configuration init can generate must be accepted by the collector
// built into it: the right component names and valid settings.
func TestGeneratedConfigsValidate(t *testing.T) {
	in := otelconfig.Instance{Platform: "ec2", InstanceID: "i-1", Region: "ap-northeast-1", OTLPEndpoint: "http://10.0.2.2:4318"}
	c := &otelconfig.Config{
		ServiceName:           "hello",
		HostMetrics:           true,
		OTLPHeaders:           map[string]string{"authorization": "Bearer x"},
		AzureConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.invalid/",
	}
	for _, choice := range []string{otelconfig.OTLP, otelconfig.AWS, otelconfig.GCP, otelconfig.Azure} {
		yaml, err := otelconfig.CollectorConfig(choice, c, in)
		if err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
		col, err := otelcol.NewCollector(Settings("yaml:" + yaml))
		if err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
		if err := col.DryRun(context.Background()); err != nil {
			t.Errorf("%s: generated configuration rejected: %v\n%s", choice, err, yaml)
		}
	}
}

// A custom configuration must be able to default BEADS_* variables that
// have no value off EC2 (${env:VAR:-default} applies only to unset ones).
func TestCustomConfigDefaultsMissingInstanceVariables(t *testing.T) {
	// Start from no BEADS_* variables at all, then set exactly what init
	// would give the collector off EC2.
	for _, k := range []string{"BEADS_SERVICE_NAME", "BEADS_PLATFORM", "BEADS_INSTANCE_ID", "BEADS_REGION", "BEADS_ZONE", "BEADS_HOSTNAME"} {
		t.Setenv(k, "") // registers restoration of the original value
		os.Unsetenv(k)
	}
	for _, kv := range otelconfig.Env(&otelconfig.Config{ServiceName: "hello"}, otelconfig.Instance{Platform: "qemu"}) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	const cfg = `
receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:4318
exporters:
  awscloudwatchlogs:
    log_group_name: /beads/${env:BEADS_SERVICE_NAME}
    log_stream_name: ${env:BEADS_INSTANCE_ID:-local}
    region: ${env:BEADS_REGION:-ap-northeast-1}
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [awscloudwatchlogs]
`
	col, err := otelcol.NewCollector(Settings("yaml:" + cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := col.DryRun(context.Background()); err != nil {
		t.Fatalf("custom configuration with :- defaults rejected: %v", err)
	}
}
