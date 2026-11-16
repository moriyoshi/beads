// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package otelconfig decides where an appliance's telemetry (logs, metrics
// and traces) goes and writes the configuration for the OpenTelemetry
// Collector embedded in init.
package otelconfig

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Destinations.
const (
	Auto  = "auto"
	None  = "none"
	OTLP  = "otlp"  // any OTLP/HTTP endpoint
	AWS   = "aws"   // CloudWatch Logs, CloudWatch metrics (EMF), X-Ray
	GCP   = "gcp"   // Cloud Logging, Cloud Monitoring, Cloud Trace
	Azure = "azure" // Azure Monitor (Application Insights)
	// Custom means the user supplied the whole collector configuration.
	Custom = "custom"
)

// Where init and the program reach the collector.
const (
	HTTPListen = "127.0.0.1:4318"
	GRPCListen = "127.0.0.1:4317"
)

// Config is the "otel" section of /etc/appliance.json.
type Config struct {
	// Exporter is one of the destinations above; empty means Auto, and a
	// ConfigFile implies Custom.
	Exporter    string `json:"exporter,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	// ConfigFile is a complete collector configuration installed in the
	// image. It may reference ${env:BEADS_*} variables (see Env).
	ConfigFile string `json:"config_file,omitempty"`
	// HostMetrics adds the host_metrics receiver (CPU, memory, file
	// systems, paging) to the metrics pipeline.
	HostMetrics bool `json:"host_metrics,omitempty"`
	// UserDataEnv passes the variables user-data gives the program to the
	// collector too, for a ConfigFile to reference as ${env:NAME}.
	UserDataEnv bool `json:"user_data_env,omitempty"`

	OTLPEndpoint          string            `json:"otlp_endpoint,omitempty"` // OTLP/HTTP base URL
	OTLPHeaders           map[string]string `json:"otlp_headers,omitempty"`
	AWSLogGroup           string            `json:"aws_log_group,omitempty"`         // default /beads/<service>
	AWSLogStream          string            `json:"aws_log_stream,omitempty"`        // default the instance ID
	AWSMetricsNamespace   string            `json:"aws_metrics_namespace,omitempty"` // default beads/<service>
	GCPLogName            string            `json:"gcp_log_name,omitempty"`          // default <service>
	AzureConnectionString string            `json:"azure_connection_string,omitempty"`
}

// Validate checks the destination name.
func (c *Config) Validate() error {
	switch c.Exporter {
	case "", Auto, None, OTLP, AWS, GCP, Azure, Custom:
	default:
		return fmt.Errorf("otel: unknown exporter %q", c.Exporter)
	}
	if c.Exporter == Custom && c.ConfigFile == "" {
		return fmt.Errorf("otel: exporter %q needs config_file", Custom)
	}
	return nil
}

// Instance is what init learned about where it runs.
type Instance struct {
	Platform   string // "ec2", "gce", "azure", "qemu", "unknown"
	InstanceID string
	Region     string
	Zone       string
	Hostname   string
	// HasCredentials reports whether the platform offers credentials for
	// its native services (an EC2 instance role, a GCE service account).
	HasCredentials bool
	// OTLPEndpoint is an endpoint named by platform metadata (QEMU fw_cfg).
	OTLPEndpoint string
}

// Resolve decides where telemetry goes. An OTLP endpoint wins wherever it
// comes from; otherwise the platform's native services are used when the
// instance can authenticate to them. Azure needs a connection string, so it
// must be configured.
func Resolve(c *Config, in Instance) string {
	switch {
	case c.ConfigFile != "":
		return Custom
	case c.Exporter != "" && c.Exporter != Auto:
		return c.Exporter
	case c.OTLPEndpoint != "" || in.OTLPEndpoint != "":
		return OTLP
	case in.Platform == "ec2" && in.HasCredentials:
		return AWS
	case in.Platform == "gce" && in.HasCredentials:
		return GCP
	case in.Platform == "azure" && c.AzureConnectionString != "":
		return Azure
	}
	return None
}

func (c *Config) service() string {
	if c.ServiceName != "" {
		return c.ServiceName
	}
	return "appliance"
}

// Signals.
const (
	Logs    = "logs"
	Metrics = "metrics"
	Traces  = "traces"
)

// exporters returns, for a destination, the collector exporters and which
// signals each one takes.
func exporters(dest string, c *Config, in Instance) (map[string]map[string]any, map[string][]string, error) {
	region := func(m map[string]any) map[string]any {
		if in.Region != "" {
			m["region"] = in.Region
		}
		return m
	}
	stream := c.AWSLogStream
	if stream == "" {
		stream = in.InstanceID
	}
	if stream == "" {
		stream = c.service()
	}
	switch dest {
	case OTLP:
		ep := c.OTLPEndpoint
		if ep == "" {
			ep = in.OTLPEndpoint
		}
		cfg := map[string]any{"endpoint": ep}
		if len(c.OTLPHeaders) > 0 {
			cfg["headers"] = c.OTLPHeaders
		}
		return map[string]map[string]any{"otlp_http": cfg},
			map[string][]string{Logs: {"otlp_http"}, Metrics: {"otlp_http"}, Traces: {"otlp_http"}}, nil
	case AWS:
		group := c.AWSLogGroup
		if group == "" {
			group = "/beads/" + c.service()
		}
		ns := c.AWSMetricsNamespace
		if ns == "" {
			ns = "beads/" + c.service()
		}
		return map[string]map[string]any{
				"awscloudwatchlogs": region(map[string]any{"log_group_name": group, "log_stream_name": stream}),
				// EMF: metrics become CloudWatch metrics through
				// structured log events in their own group.
				"awsemf": region(map[string]any{
					"namespace":       ns,
					"log_group_name":  group + "/metrics",
					"log_stream_name": stream,
					// EMF wants deltas, and by default the exporter
					// drops each counter's first cumulative point as a
					// baseline. The program's counters start at zero
					// when it starts, so that point is a valid delta;
					// without it a short-lived or rarely-changing
					// counter never reaches CloudWatch.
					"retain_initial_value_of_delta_metric": true,
				}),
				"awsxray": region(map[string]any{}),
			},
			map[string][]string{Logs: {"awscloudwatchlogs"}, Metrics: {"awsemf"}, Traces: {"awsxray"}}, nil
	case GCP:
		name := c.GCPLogName
		if name == "" {
			name = c.service()
		}
		// The project comes from the instance's service account.
		return map[string]map[string]any{"googlecloud": {"log": map[string]any{"default_log_name": name}}},
			map[string][]string{Logs: {"googlecloud"}, Metrics: {"googlecloud"}, Traces: {"googlecloud"}}, nil
	case Azure:
		if c.AzureConnectionString == "" {
			return nil, nil, fmt.Errorf("otel: %s needs azure_connection_string", Azure)
		}
		return map[string]map[string]any{"azuremonitor": {"connection_string": c.AzureConnectionString}},
			map[string][]string{Logs: {"azuremonitor"}, Metrics: {"azuremonitor"}, Traces: {"azuremonitor"}}, nil
	}
	return nil, nil, fmt.Errorf("otel: no collector exporters for %q", dest)
}

// selfBatching lists exporters that batch on their own; putting the batch
// processor in front of them batches twice (and the collector warns).
var selfBatching = map[string]bool{"awscloudwatchlogs": true}

// HostMetricsScrapers are the hostmetrics scrapers that produce data on
// NetBSD. The others do not: disk and load are "not implemented" in
// gopsutil, network runs netstat(1), which the image lacks, and processes
// reports nothing.
var HostMetricsScrapers = []string{"cpu", "filesystem", "memory", "paging"}

// CollectorConfig returns the collector configuration (JSON, which is valid
// YAML) for a destination other than None and Custom: OTLP receivers for
// all three signals, optionally host metrics, and the destination's
// exporters.
func CollectorConfig(dest string, c *Config, in Instance) (string, error) {
	exps, bySignal, err := exporters(dest, c, in)
	if err != nil {
		return "", err
	}
	receivers := map[string]any{
		"otlp": map[string]any{"protocols": map[string]any{
			"http": map[string]any{"endpoint": HTTPListen},
			"grpc": map[string]any{"endpoint": GRPCListen},
		}},
	}
	metricsReceivers := []string{"otlp"}
	if c.HostMetrics {
		scrapers := map[string]any{}
		for _, s := range HostMetricsScrapers {
			scrapers[s] = map[string]any{}
		}
		receivers["host_metrics"] = map[string]any{"collection_interval": "60s", "scrapers": scrapers}
		metricsReceivers = append(metricsReceivers, "host_metrics")
	}
	exporterCfg := map[string]any{}
	for name, cfg := range exps {
		exporterCfg[name] = cfg
	}
	pipelines := map[string]any{}
	for _, sig := range []string{Logs, Metrics, Traces} {
		recv := []string{"otlp"}
		if sig == Metrics {
			recv = metricsReceivers
		}
		procs := []string{"memory_limiter", "batch"}
		if selfBatching[bySignal[sig][0]] {
			procs = []string{"memory_limiter"}
		}
		pipelines[sig] = map[string]any{
			"receivers":  recv,
			"processors": procs,
			"exporters":  bySignal[sig],
		}
	}
	cfg := map[string]any{
		"receivers": receivers,
		"processors": map[string]any{
			"memory_limiter": map[string]any{"check_interval": "1s", "limit_percentage": 20, "spike_limit_percentage": 5},
			"batch":          map[string]any{},
		},
		"exporters": exporterCfg,
		"service": map[string]any{
			// The collector's own logs go to the console; keep them to
			// problems. It has no process metrics on NetBSD.
			"telemetry": map[string]any{
				"logs":    map[string]any{"level": "warn"},
				"metrics": map[string]any{"level": "none"},
			},
			"pipelines": pipelines,
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func (in Instance) resourceAttrs(service string) [][2]string {
	attrs := [][2]string{{"service.name", service}, {"os.type", "netbsd"}}
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, [2]string{k, v})
		}
	}
	add("host.name", in.Hostname)
	add("host.id", in.InstanceID)
	if p, ok := CloudProviders[in.Platform]; ok {
		add("cloud.provider", p[0])
		add("cloud.platform", p[1])
	}
	add("cloud.region", in.Region)
	add("cloud.availability_zone", in.Zone)
	return attrs
}

// CloudProviders maps platforms to the semantic-convention cloud.provider
// and cloud.platform values.
var CloudProviders = map[string][2]string{
	"ec2":   {"aws", "aws_ec2"},
	"gce":   {"gcp", "gcp_compute_engine"},
	"azure": {"azure", "azure_vm"},
}

// ResourceAttrs returns the resource attributes init attaches to its own
// records and hands to the program, in a stable order.
func ResourceAttrs(c *Config, in Instance) [][2]string {
	return in.resourceAttrs(c.service())
}

// AppEnv returns the standard OpenTelemetry SDK variables that point the
// program at the local collector with the instance's resource attributes,
// so an instrumented program needs no configuration of its own.
func AppEnv(c *Config, in Instance) map[string]string {
	var res []string
	for _, kv := range in.resourceAttrs(c.service()) {
		if kv[0] == "service.name" {
			continue // OTEL_SERVICE_NAME carries it
		}
		res = append(res, kv[0]+"="+kv[1])
	}
	return map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + HTTPListen,
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_SERVICE_NAME":           c.service(),
		"OTEL_RESOURCE_ATTRIBUTES":    strings.Join(res, ","),
	}
}

// Env returns BEADS_* variables describing the instance, for user
// configurations to reference as ${env:BEADS_INSTANCE_ID} and so on.
// BEADS_OTLP_ENDPOINT is the configured or platform-named (QEMU fw_cfg)
// OTLP/HTTP endpoint.
// Variables without a value are left out rather than set empty: the
// collector applies a ${env:VAR:-default} default only to unset variables,
// so off EC2 (no instance ID or region) a custom configuration can still
// fall back to its own defaults.
func Env(c *Config, in Instance) []string {
	ep := c.OTLPEndpoint
	if ep == "" {
		ep = in.OTLPEndpoint
	}
	m := map[string]string{
		"BEADS_OTLP_ENDPOINT": ep,
		"BEADS_SERVICE_NAME":  c.service(),
		"BEADS_PLATFORM":      in.Platform,
		"BEADS_INSTANCE_ID":   in.InstanceID,
		"BEADS_REGION":        in.Region,
		"BEADS_ZONE":          in.Zone,
		"BEADS_HOSTNAME":      in.Hostname,
	}
	var out []string
	for k, v := range m {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// Describe summarizes the choice for the console, without secrets.
func Describe(dest string, c *Config, in Instance) string {
	switch dest {
	case Custom:
		return "custom collector configuration " + c.ConfigFile
	case None:
		return "none"
	}
	exps, bySignal, err := exporters(dest, c, in)
	if err != nil {
		return err.Error()
	}
	var parts []string
	for _, sig := range []string{Logs, Metrics, Traces} {
		var es []string
		for _, name := range bySignal[sig] {
			var kv []string
			for k, v := range exps[name] {
				if k == "connection_string" || k == "headers" {
					continue // secrets
				}
				kv = append(kv, fmt.Sprintf("%s=%v", k, v))
			}
			sort.Strings(kv)
			if len(kv) > 0 {
				name += "(" + strings.Join(kv, " ") + ")"
			}
			es = append(es, name)
		}
		parts = append(parts, sig+"→"+strings.Join(es, ","))
	}
	if c.HostMetrics {
		parts = append(parts, "host metrics on")
	}
	return strings.Join(parts, "; ")
}
