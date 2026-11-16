// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

type fakeLogs struct {
	events        map[string][]string // stream -> messages
	deleted       []string
	groupsDeleted []string
	otherStreams  bool
}

func (f *fakeLogs) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	var out cloudwatchlogs.FilterLogEventsOutput
	for _, m := range f.events[in.LogStreamNames[0]] {
		out.Events = append(out.Events, cwltypes.FilteredLogEvent{Message: aws.String(m)})
	}
	return &out, nil
}

func (f *fakeLogs) DeleteLogStream(_ context.Context, in *cloudwatchlogs.DeleteLogStreamInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DeleteLogStreamOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.LogGroupName)+"/"+aws.ToString(in.LogStreamName))
	return &cloudwatchlogs.DeleteLogStreamOutput{}, nil
}

func (f *fakeLogs) DescribeLogStreams(context.Context, *cloudwatchlogs.DescribeLogStreamsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error) {
	var out cloudwatchlogs.DescribeLogStreamsOutput
	if f.otherStreams {
		out.LogStreams = []cwltypes.LogStream{{LogStreamName: aws.String("other")}}
	}
	return &out, nil
}

func (f *fakeLogs) DeleteLogGroup(_ context.Context, in *cloudwatchlogs.DeleteLogGroupInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DeleteLogGroupOutput, error) {
	f.groupsDeleted = append(f.groupsDeleted, aws.ToString(in.LogGroupName))
	return &cloudwatchlogs.DeleteLogGroupOutput{}, nil
}

func TestLogsCheck(t *testing.T) {
	f := &fakeLogs{events: map[string][]string{"i-1": {`{"message":"hello: listening on :8080"}`, `{"message":"hello: stopped"}`}}}
	lc := &LogsCheck{API: f, Group: "/beads/hello", Expect: []string{"listening on :8080", "hello: stopped"}}
	if err := lc.Check(context.Background(), "i-1", time.Now(), func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if err := lc.Cleanup(context.Background(), "i-1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.deleted, []string{"/beads/hello/i-1"}) || !reflect.DeepEqual(f.groupsDeleted, []string{"/beads/hello"}) {
		t.Errorf("deleted streams %v, groups %v", f.deleted, f.groupsDeleted)
	}
}

func TestLogsCheckKeepsSharedGroup(t *testing.T) {
	f := &fakeLogs{otherStreams: true}
	lc := &LogsCheck{API: f, Group: "/beads/hello"}
	if err := lc.Cleanup(context.Background(), "i-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.groupsDeleted) != 0 {
		t.Errorf("deleted a group still holding other streams: %v", f.groupsDeleted)
	}
}

func TestLogsCheckMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	f := &fakeLogs{events: map[string][]string{"i-1": {"something else"}}}
	lc := &LogsCheck{API: f, Group: "/g", Expect: []string{"hello: stopped"}}
	if err := lc.Check(ctx, "i-1", time.Now(), func(string, ...any) {}); err == nil {
		t.Fatal("expected an error for a missing line")
	}
}

// fakeIAM fails at a chosen call and records the calls made.
type fakeIAM struct {
	IAMAPI
	failAt string
	calls  []string
}

func (f *fakeIAM) call(name string) error {
	f.calls = append(f.calls, name)
	if name == f.failAt {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeIAM) CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{}, f.call("CreateRole")
}
func (f *fakeIAM) PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, f.call("PutRolePolicy")
}
func (f *fakeIAM) CreateInstanceProfile(context.Context, *iam.CreateInstanceProfileInput, ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	return &iam.CreateInstanceProfileOutput{}, f.call("CreateInstanceProfile")
}
func (f *fakeIAM) AddRoleToInstanceProfile(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, f.call("AddRoleToInstanceProfile")
}
func (f *fakeIAM) RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, f.call("RemoveRoleFromInstanceProfile")
}
func (f *fakeIAM) DeleteInstanceProfile(context.Context, *iam.DeleteInstanceProfileInput, ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, f.call("DeleteInstanceProfile")
}
func (f *fakeIAM) DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, f.call("DeleteRolePolicy")
}
func (f *fakeIAM) DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, f.call("DeleteRole")
}

func TestTempTelemetryProfileUnwindsPartialFailure(t *testing.T) {
	f := &fakeIAM{failAt: "CreateInstanceProfile"}
	cleanup, err := TempTelemetryProfile(context.Background(), f, "beads-smoke-1", nil)
	if err == nil || !strings.Contains(err.Error(), "CreateInstanceProfile") {
		t.Fatalf("err = %v", err)
	}
	f.calls = nil
	if err := cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Only what was created is removed, in reverse order.
	if want := []string{"DeleteRolePolicy", "DeleteRole"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("cleanup calls %v, want %v", f.calls, want)
	}
}

func TestTempTelemetryProfileFullCleanup(t *testing.T) {
	f := &fakeIAM{}
	cleanup, err := TempTelemetryProfile(context.Background(), f, "beads-smoke-1", map[string]string{"beads": "smoke-test"})
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"RemoveRoleFromInstanceProfile", "DeleteInstanceProfile", "DeleteRolePolicy", "DeleteRole"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("cleanup calls %v, want %v", f.calls, want)
	}
}
