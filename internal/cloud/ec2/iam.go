// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// IAMAPI is the subset of the IAM client used for temporary instance
// profiles.
type IAMAPI interface {
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	CreateInstanceProfile(context.Context, *iam.CreateInstanceProfileInput, ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(context.Context, *iam.DeleteInstanceProfileInput, ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
	DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

const ec2TrustPolicy = `{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Principal": {"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}]
}`

// telemetryPolicy allows what the collector's AWS exporters need:
// awscloudwatchlogs and awsemf write CloudWatch Logs, awsxray writes X-Ray.
const telemetryPolicy = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents",
      "logs:DescribeLogStreams", "logs:PutRetentionPolicy",
      "xray:PutTraceSegments", "xray:PutTelemetryRecords",
      "xray:GetSamplingRules", "xray:GetSamplingTargets"
    ],
    "Resource": "*"
  }]
}`

const telemetryPolicyName = "beads-telemetry-write"

// TempTelemetryProfile creates a role and instance profile, both called
// name, that let an instance write logs, EMF metrics and X-Ray traces. The
// returned cleanup deletes both; it is safe to call after a partial failure.
func TempTelemetryProfile(ctx context.Context, api IAMAPI, name string, tags map[string]string) (cleanup func(context.Context) error, err error) {
	var iamTags []iamtypes.Tag
	for k, v := range tags {
		iamTags = append(iamTags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	var (
		roleCreated, policyPut, profileCreated, roleAdded bool
	)
	cleanup = func(ctx context.Context) error {
		var errs []error
		if roleAdded {
			if _, err := api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(name)}); err != nil {
				errs = append(errs, fmt.Errorf("RemoveRoleFromInstanceProfile %s: %w", name, err))
			}
		}
		if profileCreated {
			if _, err := api.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)}); err != nil {
				errs = append(errs, fmt.Errorf("DeleteInstanceProfile %s: %w", name, err))
			}
		}
		if policyPut {
			if _, err := api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(telemetryPolicyName)}); err != nil {
				errs = append(errs, fmt.Errorf("DeleteRolePolicy %s: %w", name, err))
			}
		}
		if roleCreated {
			if _, err := api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil {
				errs = append(errs, fmt.Errorf("DeleteRole %s: %w", name, err))
			}
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (func(context.Context) error, error) {
		return cleanup, err
	}
	if _, err := api.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(ec2TrustPolicy),
		Description:              aws.String("temporary: beads smoke test (CloudWatch Logs, EMF, X-Ray)"),
		Tags:                     iamTags,
	}); err != nil {
		return fail(fmt.Errorf("CreateRole %s: %w", name, err))
	}
	roleCreated = true
	if _, err := api.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(name),
		PolicyName:     aws.String(telemetryPolicyName),
		PolicyDocument: aws.String(telemetryPolicy),
	}); err != nil {
		return fail(fmt.Errorf("PutRolePolicy %s: %w", name, err))
	}
	policyPut = true
	if _, err := api.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(name),
		Tags:                iamTags,
	}); err != nil {
		return fail(fmt.Errorf("CreateInstanceProfile %s: %w", name, err))
	}
	profileCreated = true
	if _, err := api.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(name),
		RoleName:            aws.String(name),
	}); err != nil {
		return fail(fmt.Errorf("AddRoleToInstanceProfile %s: %w", name, err))
	}
	roleAdded = true
	return cleanup, nil
}
