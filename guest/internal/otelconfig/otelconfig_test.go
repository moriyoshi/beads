// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package otelconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	ec2 := Instance{Platform: "ec2", InstanceID: "i-1", Region: "ap-northeast-1", HasCredentials: true}
	for _, tc := range []struct {
		name string
		cfg  Config
		in   Instance
		want string
	}{
		{"ec2 with a role", Config{}, ec2, AWS},
		{"ec2 without a role", Config{}, Instance{Platform: "ec2"}, None},
		{"gce", Config{}, Instance{Platform: "gce", HasCredentials: true}, GCP},
		{"azure unconfigured", Config{}, Instance{Platform: "azure"}, None},
		{"azure configured", Config{AzureConnectionString: "InstrumentationKey=x"}, Instance{Platform: "azure"}, Azure},
		{"otlp endpoint beats cloud", Config{OTLPEndpoint: "https://c:4318"}, ec2, OTLP},
		{"qemu fw_cfg endpoint", Config{}, Instance{Platform: "qemu", OTLPEndpoint: "http://10.0.2.2:1"}, OTLP},
		{"explicit none", Config{Exporter: None}, ec2, None},
		{"config file", Config{ConfigFile: "/etc/otelcol/config.yaml"}, ec2, Custom},
	} {
		if got := Resolve(&tc.cfg, tc.in); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

type pipeline struct {
	Receivers []string `json:"receivers"`
	Exporters []string `json:"exporters"`
}

func pipelines(t *testing.T, s string) map[string]pipeline {
	t.Helper()
	var m struct {
		Service struct {
			Pipelines map[string]pipeline `json:"pipelines"`
		} `json:"service"`
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m.Service.Pipelines
}

func TestCollectorConfigAWS(t *testing.T) {
	c := &Config{ServiceName: "hello", HostMetrics: true}
	in := Instance{Platform: "ec2", InstanceID: "i-0abc", Region: "ap-northeast-1"}
	s, err := CollectorConfig(AWS, c, in)
	if err != nil {
		t.Fatal(err)
	}
	p := pipelines(t, s)
	if !reflect.DeepEqual(p[Logs].Exporters, []string{"awscloudwatchlogs"}) ||
		!reflect.DeepEqual(p[Metrics].Exporters, []string{"awsemf"}) ||
		!reflect.DeepEqual(p[Traces].Exporters, []string{"awsxray"}) {
		t.Errorf("pipelines %+v", p)
	}
	if !reflect.DeepEqual(p[Metrics].Receivers, []string{"otlp", "host_metrics"}) {
		t.Errorf("metrics receivers %v", p[Metrics].Receivers)
	}
	var m map[string]map[string]map[string]any
	json.Unmarshal([]byte(s), &m)
	if g := m["exporters"]["awscloudwatchlogs"]; g["log_group_name"] != "/beads/hello" || g["log_stream_name"] != "i-0abc" || g["region"] != "ap-northeast-1" {
		t.Errorf("awscloudwatchlogs %v", g)
	}
	if e := m["exporters"]["awsemf"]; e["namespace"] != "beads/hello" || e["log_group_name"] != "/beads/hello/metrics" || e["retain_initial_value_of_delta_metric"] != true {
		t.Errorf("awsemf %v", e)
	}
	if !strings.Contains(s, `"processors": [
          "memory_limiter"
        ],
        "receivers": [
          "otlp"
        ]`) {
		t.Errorf("logs pipeline to awscloudwatchlogs should not batch twice:\n%s", s)
	}
	if !strings.Contains(s, `"127.0.0.1:4317"`) || !strings.Contains(s, `"127.0.0.1:4318"`) {
		t.Error("receiver endpoints missing")
	}
	d := Describe(AWS, c, in)
	for _, want := range []string{"logs→awscloudwatchlogs(", "metrics→awsemf(", "traces→awsxray(", "host metrics on"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe = %q lacks %q", d, want)
		}
	}
}

func TestCollectorConfigOTLPAllSignals(t *testing.T) {
	s, err := CollectorConfig(OTLP, &Config{}, Instance{OTLPEndpoint: "http://10.0.2.2:1"})
	if err != nil {
		t.Fatal(err)
	}
	for sig, p := range pipelines(t, s) {
		if !reflect.DeepEqual(p.Exporters, []string{"otlp_http"}) || !reflect.DeepEqual(p.Receivers, []string{"otlp"}) {
			t.Errorf("%s: %+v", sig, p)
		}
	}
}

func TestEnvOmitsEmptyValues(t *testing.T) {
	// Off EC2 there is no instance ID, region or zone.
	got := Env(&Config{ServiceName: "hello"}, Instance{Platform: "qemu", Hostname: "hello-image"})
	want := []string{"BEADS_HOSTNAME=hello-image", "BEADS_PLATFORM=qemu", "BEADS_SERVICE_NAME=hello"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env = %v, want %v", got, want)
	}
	got = Env(&Config{ServiceName: "hello"}, Instance{Platform: "qemu", OTLPEndpoint: "http://10.0.2.2:4318"})
	want = []string{"BEADS_OTLP_ENDPOINT=http://10.0.2.2:4318", "BEADS_PLATFORM=qemu", "BEADS_SERVICE_NAME=hello"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env = %v, want %v", got, want)
	}
}

func TestAppEnv(t *testing.T) {
	env := AppEnv(&Config{ServiceName: "hello"}, Instance{Platform: "ec2", InstanceID: "i-1", Region: "r"})
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:4318" || env["OTEL_SERVICE_NAME"] != "hello" {
		t.Errorf("env %v", env)
	}
	if got := env["OTEL_RESOURCE_ATTRIBUTES"]; got != "os.type=netbsd,host.id=i-1,cloud.provider=aws,cloud.platform=aws_ec2,cloud.region=r" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q", got)
	}
}

func TestDescribeHidesSecrets(t *testing.T) {
	c := &Config{AzureConnectionString: "InstrumentationKey=secret"}
	if d := Describe(Azure, c, Instance{}); strings.Contains(d, "secret") {
		t.Errorf("Describe leaks the connection string: %q", d)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []Config{{Exporter: "syslog"}, {Exporter: Custom}} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v validated", c)
		}
	}
}
