// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command cloud publishes appliance images to public clouds and smoke-tests
// them there.
//
//	cloud ec2-publish --image disk.raw --name my-appliance [--region R] [--profile P]
//	cloud ec2-smoke   (--image disk.raw | --ami ami-...) [--http-port 8080 --http-expect hello]
//
// Every flag has an environment-variable default (CLOUD_*), which is how the
// Bazel rules configure it; flags given on the command line (after `--` with
// `bazel run`) take precedence. Credentials come from the usual AWS SDK
// sources: AWS_PROFILE, environment variables, SSO, instance roles.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/xray"

	cloudec2 "github.com/moriyoshi/beads/internal/cloud/ec2"
	"github.com/moriyoshi/beads/internal/runfiles"
)

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cloud: "+format+"\n", args...)
}

// tagFlag collects repeated --tag key=value flags.
type tagFlag map[string]string

func (t tagFlag) String() string { return fmt.Sprint(map[string]string(t)) }

func (t tagFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("tag %q is not key=value", s)
	}
	t[k] = v
	return nil
}

func main() {
	// The subcommand is the first argument, or $CLOUD_COMMAND when the
	// arguments start with a flag (as with `bazel run :x.publish_ec2 -- --region R`).
	cmd := envOr("CLOUD_COMMAND", "")
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" {
		fmt.Fprintln(os.Stderr, "usage: cloud {ec2-publish|ec2-smoke} [flags]")
		os.Exit(2)
	}
	var err error
	switch cmd {
	case "ec2-publish":
		err = ec2Publish(args)
	case "ec2-smoke":
		err = ec2Smoke(args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}

type awsFlags struct {
	region  *string
	profile *string
}

// defaultInstanceType is the smallest Nitro type for each architecture.
func defaultInstanceType(arch string) string {
	if arch == "aarch64" {
		return "t4g.nano"
	}
	return "t3.nano"
}

func addAWSFlags(fs *flag.FlagSet) awsFlags {
	return awsFlags{
		region:  fs.String("region", envOr("CLOUD_AWS_REGION", ""), "AWS region (default: from the profile or AWS_REGION)"),
		profile: fs.String("profile", envOr("CLOUD_AWS_PROFILE", ""), "AWS shared-config profile (default: AWS_PROFILE)"),
	}
}

// load resolves credentials and prints which account and region the command
// will act on, so a wrong profile is noticed before anything is created.
func (f awsFlags) load(ctx context.Context) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if *f.region != "" {
		opts = append(opts, awsconfig.WithRegion(*f.region))
	}
	if *f.profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(*f.profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return cfg, err
	}
	if cfg.Region == "" {
		return cfg, errors.New("no AWS region; pass --region or set AWS_REGION")
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return cfg, fmt.Errorf("checking AWS credentials: %w", err)
	}
	logf("AWS account %s (%s), region %s", aws.ToString(id.Account), aws.ToString(id.Arn), cfg.Region)
	return cfg, nil
}

func resolveImage(p string) (string, error) {
	if p == "" {
		return "", errors.New("no image given (--image or $CLOUD_IMAGE)")
	}
	return runfiles.Resolve(p)
}

func defaultName(base string) string {
	if base == "" {
		base = "netbsd-appliance"
	}
	return fmt.Sprintf("%s-%s", base, time.Now().UTC().Format("20060102-150405"))
}

func ec2Publish(args []string) error {
	fs := flag.NewFlagSet("ec2-publish", flag.ExitOnError)
	af := addAWSFlags(fs)
	image := fs.String("image", envOr("CLOUD_IMAGE", ""), "raw disk image")
	name := fs.String("name", "", "AMI name (default: --name-prefix plus a UTC timestamp)")
	prefix := fs.String("name-prefix", envOr("CLOUD_NAME", ""), "AMI name prefix")
	desc := fs.String("description", envOr("CLOUD_DESCRIPTION", ""), "AMI description")
	size := fs.Int64("volume-size", 0, "root volume size in GiB (default: image size rounded up)")
	arch := fs.String("arch", envOr("CLOUD_ARCH", "amd64"), "image architecture: amd64 or aarch64")
	out := fs.String("out", "", "write the result as JSON to this file")
	tags := tagFlag{}
	fs.Var(tags, "tag", "key=value tag for the snapshot and AMI (repeatable)")
	fs.Parse(args)

	img, err := resolveImage(*image)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = defaultName(*prefix)
	}
	ctx := context.Background()
	cfg, err := af.load(ctx)
	if err != nil {
		return err
	}
	tags["beads"] = filepath.Base(img)
	p, err := cloudec2.Publish(ctx, ebs.NewFromConfig(cfg), ec2.NewFromConfig(cfg), cfg.Region, img,
		cloudec2.SnapshotOptions{Description: *name, VolumeSizeGiB: *size, Tags: tags, Progress: progress()},
		cloudec2.AMIOptions{Name: *name, Description: *desc, Tags: tags, Arch: *arch}, logf)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		return os.WriteFile(*out, append(b, '\n'), 0o644)
	}
	return nil
}

// progress returns a callback that reports upload progress every 10%.
func progress() func(done, total int) {
	last := -1
	return func(done, total int) {
		if pct := done * 10 / max(total, 1); pct != last {
			last = pct
			logf("uploaded %d blocks (%d%% of the image's extent)", done, pct*10)
		}
	}
}

// probeCIDR returns this machine's public IPv4 address as a /32.
func probeCIDR(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("finding this machine's public IP: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)) + "/32", nil
}

func ec2Smoke(args []string) error {
	fs := flag.NewFlagSet("ec2-smoke", flag.ExitOnError)
	af := addAWSFlags(fs)
	image := fs.String("image", envOr("CLOUD_IMAGE", ""), "raw disk image to publish for the test (ignored with --ami)")
	ami := fs.String("ami", "", "existing AMI to test instead of publishing --image")
	prefix := fs.String("name-prefix", envOr("CLOUD_NAME", ""), "name prefix for the temporary AMI")
	keep := fs.Bool("keep-image", false, "keep the AMI and snapshot published for the test")
	arch := fs.String("arch", envOr("CLOUD_ARCH", "amd64"), "image architecture: amd64 or aarch64")
	itype := fs.String("instance-type", envOr("CLOUD_EC2_INSTANCE_TYPE", ""), "instance type (default: t3.nano, or t4g.nano for aarch64)")
	subnet := fs.String("subnet", envOr("CLOUD_EC2_SUBNET", ""), "subnet to launch in (default: the default VPC)")
	ready := fs.String("ready", envOr("CLOUD_READY", "nbinit: ready"), "console line marking a booted guest")
	port := fs.Int("http-port", envInt("CLOUD_HTTP_PORT", 0), "guest TCP port to probe (0 = skip)")
	path := fs.String("http-path", envOr("CLOUD_HTTP_PATH", "/"), "path to GET")
	expect := fs.String("http-expect", envOr("CLOUD_HTTP_EXPECT", ""), "substring the response must contain")
	userDataFile := fs.String("user-data", envOr("CLOUD_USER_DATA", ""), "file to launch the instance with as user-data (path or runfiles location)")
	timeout := fs.Duration("timeout", 10*time.Minute, "deadline for the instance part of the test")
	telemetry := fs.Bool("telemetry-check", envOr("CLOUD_TELEMETRY_CHECK", "") == "1",
		"give the instance a temporary IAM role for CloudWatch and X-Ray, and check after it terminates that its telemetry arrived")
	service := fs.String("service", envOr("CLOUD_SERVICE", ""), "the appliance's OpenTelemetry service.name (log groups /beads/<service>)")
	logsExpect := fs.String("logs-expect", envOr("CLOUD_LOGS_EXPECT", ""), "|-separated substrings that must reach CloudWatch Logs")
	metricsExpect := fs.String("metrics-expect", envOr("CLOUD_METRICS_EXPECT", ""), "|-separated metric names that must reach CloudWatch (as EMF events)")
	tracesCheck := fs.Bool("traces-check", envOr("CLOUD_TRACES_CHECK", "") == "1", "require a trace for the service in X-Ray")
	fs.Parse(args)

	if *itype == "" {
		*itype = defaultInstanceType(*arch)
	}
	var userData []byte
	if *userDataFile != "" {
		p, err := runfiles.Resolve(*userDataFile)
		if err != nil {
			return err
		}
		if userData, err = os.ReadFile(p); err != nil {
			return err
		}
	}
	// Bazel sends SIGTERM when a test times out, and people press Ctrl-C.
	// Cancelling ctx makes Smoke fail fast, and its cleanup (which runs on a
	// context detached from this one) still terminates the instance and
	// deletes everything it created.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := af.load(ctx)
	if err != nil {
		return err
	}
	api := ec2.NewFromConfig(cfg)
	tags := map[string]string{"beads": "smoke-test"}

	var instanceProfile string
	var checks []cloudec2.PostCheck
	if *telemetry {
		if *service == "" {
			return errors.New("--telemetry-check needs --service")
		}
		// Created first so that IAM has propagated by the time the image is
		// published and the instance launched.
		instanceProfile = fmt.Sprintf("beads-smoke-%d", time.Now().Unix())
		cleanupIAM, err := cloudec2.TempTelemetryProfile(ctx, iam.NewFromConfig(cfg), instanceProfile, tags)
		defer func() {
			if cerr := cleanupIAM(context.Background()); cerr != nil {
				logf("cleanup: %v", cerr)
			} else {
				logf("deleted IAM role and instance profile %s", instanceProfile)
			}
		}()
		if err != nil {
			return err
		}
		logf("created IAM role and instance profile %s (CloudWatch Logs, EMF, X-Ray)", instanceProfile)
		logsAPI := cloudwatchlogs.NewFromConfig(cfg)
		split := func(v string) []string {
			var out []string
			for _, x := range strings.Split(v, "|") {
				if x != "" {
					out = append(out, x)
				}
			}
			return out
		}
		if want := split(*logsExpect); len(want) > 0 {
			checks = append(checks, &cloudec2.LogsCheck{API: logsAPI, Group: "/beads/" + *service, Expect: want})
		}
		if want := split(*metricsExpect); len(want) > 0 {
			// awsemf writes each metric as an EMF event naming it.
			checks = append(checks, &cloudec2.LogsCheck{API: logsAPI, Group: "/beads/" + *service + "/metrics", Expect: want})
		}
		if *tracesCheck {
			checks = append(checks, &cloudec2.XRayCheck{API: xray.NewFromConfig(cfg), Service: *service})
		}
	}

	imageID := *ami
	if imageID == "" {
		img, err := resolveImage(*image)
		if err != nil {
			return err
		}
		name := defaultName(strings.TrimSuffix(*prefix, "-") + "-smoke")
		p, err := cloudec2.Publish(ctx, ebs.NewFromConfig(cfg), api, cfg.Region, img,
			cloudec2.SnapshotOptions{Description: name, Tags: tags},
			cloudec2.AMIOptions{Name: name, Tags: tags, Arch: *arch}, logf)
		if p != nil && !*keep {
			defer func() {
				if uerr := cloudec2.Unpublish(context.Background(), api, p); uerr != nil {
					logf("cleanup: %v", uerr)
				} else {
					logf("deleted %s and %s", p.ImageID, p.SnapshotID)
				}
			}()
		}
		if err != nil {
			return err
		}
		imageID = p.ImageID
	}

	opt := cloudec2.SmokeOptions{
		ImageID: imageID, InstanceType: *itype, SubnetID: *subnet,
		ReadyMarker: *ready, HTTPPort: *port, HTTPPath: *path, HTTPExpect: *expect,
		Timeout: *timeout, Tags: tags, Logf: logf,
		InstanceProfile: instanceProfile, Checks: checks, UserData: userData,
	}
	if *port != 0 {
		if opt.ProbeCIDR, err = probeCIDR(ctx); err != nil {
			return err
		}
	}
	if d := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); d != "" {
		if f, err := os.Create(filepath.Join(d, "ec2-console.log")); err == nil {
			defer f.Close()
			opt.Console = f
		}
	}
	if err := cloudec2.Smoke(ctx, api, opt); err != nil {
		return err
	}
	logf("PASS: %s booted on %s", imageID, *itype)
	return nil
}
