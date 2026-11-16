// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/xray"
)

// SmokeAPI is the subset of the EC2 client the smoke test uses.
type SmokeAPI interface {
	ec2.DescribeInstancesAPIClient
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	GetConsoleOutput(context.Context, *ec2.GetConsoleOutputInput, ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
}

// SmokeOptions controls a smoke test.
type SmokeOptions struct {
	ImageID      string
	InstanceType string // default t3.nano
	// SubnetID, if set, launches there with a public IP; otherwise the
	// default VPC's default subnet is used.
	SubnetID string
	// ReadyMarker is the console line that marks a booted guest.
	ReadyMarker string
	// HTTPPort, if non-zero, is opened to ProbeCIDR and probed.
	HTTPPort   int
	HTTPPath   string
	HTTPExpect string
	ProbeCIDR  string // the tester's address, e.g. "203.0.113.7/32"
	Timeout    time.Duration
	Tags       map[string]string
	Logf       func(string, ...any)
	// Console, if set, receives the instance's console output once the test
	// ends.
	Console io.Writer
	// InstanceProfile, if set, is attached to the instance.
	InstanceProfile string
	// UserData, if set, is the instance's user-data (at most 16 KiB).
	UserData []byte
	// Checks run once the instance has terminated: the guest flushes its
	// telemetry while shutting down.
	Checks []PostCheck
}

// PostCheck verifies something the instance should have produced, after it
// terminated, and removes what it can of it.
type PostCheck interface {
	Check(ctx context.Context, instanceID string, launched time.Time, logf func(string, ...any)) error
	Cleanup(ctx context.Context, instanceID string) error
}

// LogsAPI is the subset of the CloudWatch Logs client the check uses.
type LogsAPI interface {
	FilterLogEvents(context.Context, *cloudwatchlogs.FilterLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
	DeleteLogStream(context.Context, *cloudwatchlogs.DeleteLogStreamInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DeleteLogStreamOutput, error)
	DescribeLogStreams(context.Context, *cloudwatchlogs.DescribeLogStreamsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error)
	DeleteLogGroup(context.Context, *cloudwatchlogs.DeleteLogGroupInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DeleteLogGroupOutput, error)
}

// LogsCheck requires substrings to have reached the instance's CloudWatch
// Logs stream (named after the instance ID, the beads default). The stream
// is deleted afterwards, and the group too if nothing else is in it.
type LogsCheck struct {
	API    LogsAPI
	Group  string
	Expect []string
}

// check polls the stream until every expected substring has appeared in
// some event, for up to two minutes (CloudWatch indexes events within
// seconds, but not instantly).
func (lc *LogsCheck) Check(ctx context.Context, stream string, _ time.Time, logf func(string, ...any)) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var messages []string
		in := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:   aws.String(lc.Group),
			LogStreamNames: []string{stream},
		}
		p := cloudwatchlogs.NewFilterLogEventsPaginator(lc.API, in)
		var err error
		for p.HasMorePages() {
			var out *cloudwatchlogs.FilterLogEventsOutput
			if out, err = p.NextPage(ctx); err != nil {
				break
			}
			for _, e := range out.Events {
				messages = append(messages, aws.ToString(e.Message))
			}
		}
		var missing []string
		if err == nil {
			for _, want := range lc.Expect {
				found := false
				for _, m := range messages {
					if strings.Contains(m, want) {
						found = true
						break
					}
				}
				if !found {
					missing = append(missing, want)
				}
			}
			if len(missing) == 0 {
				logf("CloudWatch Logs %s/%s has all %d expected lines (%d events)", lc.Group, stream, len(lc.Expect), len(messages))
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("reading CloudWatch Logs %s/%s: %w", lc.Group, stream, err)
			}
			return fmt.Errorf("CloudWatch Logs %s/%s lacks %q (%d events)", lc.Group, stream, missing, len(messages))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (lc *LogsCheck) Cleanup(ctx context.Context, stream string) error {
	_, err := lc.API.DeleteLogStream(ctx, &cloudwatchlogs.DeleteLogStreamInput{
		LogGroupName:  aws.String(lc.Group),
		LogStreamName: aws.String(stream),
	})
	var notFound *cwltypes.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("DeleteLogStream %s/%s: %w", lc.Group, stream, err)
	}
	out, err := lc.API.DescribeLogStreams(ctx, &cloudwatchlogs.DescribeLogStreamsInput{LogGroupName: aws.String(lc.Group), Limit: aws.Int32(1)})
	if err != nil || len(out.LogStreams) > 0 {
		return nil // in use by someone else, or unknown: leave the group
	}
	if _, err := lc.API.DeleteLogGroup(ctx, &cloudwatchlogs.DeleteLogGroupInput{LogGroupName: aws.String(lc.Group)}); err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("DeleteLogGroup %s: %w", lc.Group, err)
	}
	return nil
}

// XRayAPI is the subset of the X-Ray client the check uses.
type XRayAPI interface {
	GetTraceSummaries(context.Context, *xray.GetTraceSummariesInput, ...func(*xray.Options)) (*xray.GetTraceSummariesOutput, error)
}

// XRayCheck requires at least one trace from the service since launch.
// Traces cannot be deleted; X-Ray expires them after 30 days.
type XRayCheck struct {
	API     XRayAPI
	Service string
}

func (xc *XRayCheck) Check(ctx context.Context, _ string, launched time.Time, logf func(string, ...any)) error {
	deadline := time.Now().Add(3 * time.Minute)
	filter := fmt.Sprintf("service(%q)", xc.Service)
	for {
		out, err := xc.API.GetTraceSummaries(ctx, &xray.GetTraceSummariesInput{
			StartTime:        aws.Time(launched.Add(-time.Minute)),
			EndTime:          aws.Time(time.Now()),
			FilterExpression: aws.String(filter),
		})
		if err == nil && len(out.TraceSummaries) > 0 {
			logf("X-Ray has %d trace(s) for service %q", len(out.TraceSummaries), xc.Service)
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("X-Ray GetTraceSummaries: %w", err)
			}
			return fmt.Errorf("X-Ray has no trace for service %q", xc.Service)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func (xc *XRayCheck) Cleanup(context.Context, string) error { return nil }

// Smoke launches an instance from the AMI, waits for the ready marker on its
// serial console, optionally probes HTTP, and tears everything down.
func Smoke(ctx context.Context, api SmokeAPI, o SmokeOptions) (err error) {
	if o.InstanceType == "" {
		o.InstanceType = "t3.nano"
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Minute
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	// Cleanup must run even after the deadline has passed.
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	}

	vpcID, err := o.vpc(ctx, api)
	if err != nil {
		return err
	}
	tags := ec2Tags(o.Tags)

	sgName := fmt.Sprintf("beads-smoke-%d", time.Now().UnixNano())
	sg, err := api.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("temporary: beads smoke test"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeSecurityGroup, Tags: tags},
		},
	})
	if err != nil {
		return fmt.Errorf("CreateSecurityGroup: %w", err)
	}
	sgID := aws.ToString(sg.GroupId)
	logf("created security group %s", sgID)
	var instanceID string
	var launched time.Time
	defer func() {
		cctx, ccancel := cleanupCtx()
		defer ccancel()
		if instanceID != "" {
			if o.Console != nil {
				if out, cerr := consoleOutput(cctx, api, instanceID); cerr == nil {
					io.WriteString(o.Console, out)
				}
			}
			logf("terminating %s", instanceID)
			if _, terr := api.TerminateInstances(cctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}}); terr != nil {
				err = errors.Join(err, fmt.Errorf("TerminateInstances %s: %w", instanceID, terr))
				return
			}
			w := ec2.NewInstanceTerminatedWaiter(api)
			if werr := w.Wait(cctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}}, 10*time.Minute); werr != nil {
				err = errors.Join(err, fmt.Errorf("waiting for %s to terminate: %w (security group %s left behind)", instanceID, werr, sgID))
				return
			}
			// Only meaningful when the instance came up; otherwise the
			// boot failure is the error to report. Every check runs, so
			// one failure does not hide another.
			booted := err == nil
			for _, pc := range o.Checks {
				if booted {
					if cerr := pc.Check(cctx, instanceID, launched, logf); cerr != nil {
						err = errors.Join(err, cerr)
					}
				}
				if cerr := pc.Cleanup(cctx, instanceID); cerr != nil {
					err = errors.Join(err, cerr)
				}
			}
		}
		if _, derr := api.DeleteSecurityGroup(cctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)}); derr != nil {
			err = errors.Join(err, fmt.Errorf("DeleteSecurityGroup %s: %w", sgID, derr))
		}
	}()

	if o.HTTPPort != 0 {
		if o.ProbeCIDR == "" {
			return errors.New("HTTPPort set without ProbeCIDR")
		}
		_, err = api.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []types.IpPermission{{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(int32(o.HTTPPort)),
				ToPort:     aws.Int32(int32(o.HTTPPort)),
				IpRanges:   []types.IpRange{{CidrIp: aws.String(o.ProbeCIDR), Description: aws.String("smoke-test prober")}},
			}},
		})
		if err != nil {
			return fmt.Errorf("AuthorizeSecurityGroupIngress: %w", err)
		}
	}

	run := &ec2.RunInstancesInput{
		ImageId:      aws.String(o.ImageID),
		InstanceType: types.InstanceType(o.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens: types.HttpTokensStateRequired,
		},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: append(tags, types.Tag{Key: aws.String("Name"), Value: aws.String(sgName)})},
		},
	}
	if o.SubnetID != "" {
		run.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			SubnetId:                 aws.String(o.SubnetID),
			Groups:                   []string{sgID},
			AssociatePublicIpAddress: aws.Bool(true),
		}}
	} else {
		run.SecurityGroupIds = []string{sgID}
	}
	if o.UserData != nil {
		run.UserData = aws.String(base64.StdEncoding.EncodeToString(o.UserData))
	}
	if o.InstanceProfile != "" {
		run.IamInstanceProfile = &types.IamInstanceProfileSpecification{Name: aws.String(o.InstanceProfile)}
	}
	var ri *ec2.RunInstancesOutput
	for {
		ri, err = api.RunInstances(ctx, run)
		// A new instance profile takes a little while to propagate.
		if err != nil && o.InstanceProfile != "" && strings.Contains(err.Error(), "Invalid IAM Instance Profile") {
			select {
			case <-ctx.Done():
				return fmt.Errorf("RunInstances: %w", err)
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break
	}
	if err != nil {
		return fmt.Errorf("RunInstances: %w", err)
	}
	instanceID = aws.ToString(ri.Instances[0].InstanceId)
	start := time.Now()
	launched = start
	logf("launched %s (%s)", instanceID, o.InstanceType)

	if err := ec2.NewInstanceRunningWaiter(api).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}}, o.Timeout); err != nil {
		return fmt.Errorf("waiting for %s to run: %w", instanceID, err)
	}
	ip, err := publicIP(ctx, api, instanceID)
	if err != nil {
		return err
	}
	logf("%s running at %s; waiting for %q on the console", instanceID, ip, o.ReadyMarker)

	for {
		out, err := consoleOutput(ctx, api, instanceID)
		if err == nil && strings.Contains(out, o.ReadyMarker) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%q not seen on the console of %s within %v", o.ReadyMarker, instanceID, o.Timeout)
		case <-time.After(10 * time.Second):
		}
	}
	logf("guest ready %v after launch", time.Since(start).Round(time.Second))

	if o.HTTPPort != 0 {
		if err := probe(ctx, fmt.Sprintf("http://%s:%d%s", ip, o.HTTPPort, o.HTTPPath), o.HTTPExpect, logf); err != nil {
			return err
		}
	}
	return nil
}

func (o *SmokeOptions) vpc(ctx context.Context, api SmokeAPI) (string, error) {
	if o.SubnetID != "" {
		out, err := api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{o.SubnetID}})
		if err != nil {
			return "", fmt.Errorf("DescribeSubnets: %w", err)
		}
		if len(out.Subnets) != 1 {
			return "", fmt.Errorf("subnet %s not found", o.SubnetID)
		}
		return aws.ToString(out.Subnets[0].VpcId), nil
	}
	out, err := api.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeVpcs: %w", err)
	}
	if len(out.Vpcs) == 0 {
		return "", errors.New("the region has no default VPC; pass a subnet with a route to the internet")
	}
	return aws.ToString(out.Vpcs[0].VpcId), nil
}

func publicIP(ctx context.Context, api SmokeAPI, id string) (string, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return "", err
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if ip := aws.ToString(i.PublicIpAddress); ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("%s has no public IP address", id)
}

func consoleOutput(ctx context.Context, api SmokeAPI, id string) (string, error) {
	out, err := api.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)})
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func probe(ctx context.Context, url, want string, logf func(string, ...any)) error {
	client := &http.Client{Timeout: 10 * time.Second}
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), want) {
				logf("GET %s -> %q", url, strings.TrimSpace(string(body)))
				return nil
			}
			last = fmt.Errorf("status %d, body %q", resp.StatusCode, body)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("GET %s: %v", url, last)
		case <-time.After(5 * time.Second):
		}
	}
}
