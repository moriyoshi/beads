// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	ebstypes "github.com/aws/aws-sdk-go-v2/service/ebs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEBS records blocks and verifies checksums like the real service.
type fakeEBS struct {
	mu       sync.Mutex
	size     int64
	blocks   map[int32][]byte
	complete *ebs.CompleteSnapshotInput
}

func (f *fakeEBS) StartSnapshot(_ context.Context, in *ebs.StartSnapshotInput, _ ...func(*ebs.Options)) (*ebs.StartSnapshotOutput, error) {
	f.size = aws.ToInt64(in.VolumeSize)
	f.blocks = map[int32][]byte{}
	return &ebs.StartSnapshotOutput{SnapshotId: aws.String("snap-fake")}, nil
}

func (f *fakeEBS) PutSnapshotBlock(_ context.Context, in *ebs.PutSnapshotBlockInput, _ ...func(*ebs.Options)) (*ebs.PutSnapshotBlockOutput, error) {
	b, err := io.ReadAll(in.BlockData)
	if err != nil {
		return nil, err
	}
	if len(b) != BlockSize || aws.ToInt32(in.DataLength) != BlockSize {
		return nil, fmt.Errorf("block %d: %d bytes", aws.ToInt32(in.BlockIndex), len(b))
	}
	sum := sha256.Sum256(b)
	if base64.StdEncoding.EncodeToString(sum[:]) != aws.ToString(in.Checksum) {
		return nil, fmt.Errorf("block %d: checksum mismatch", aws.ToInt32(in.BlockIndex))
	}
	f.mu.Lock()
	f.blocks[aws.ToInt32(in.BlockIndex)] = b
	f.mu.Unlock()
	return &ebs.PutSnapshotBlockOutput{}, nil
}

func (f *fakeEBS) CompleteSnapshot(_ context.Context, in *ebs.CompleteSnapshotInput, _ ...func(*ebs.Options)) (*ebs.CompleteSnapshotOutput, error) {
	f.complete = in
	return &ebs.CompleteSnapshotOutput{Status: ebstypes.StatusCompleted}, nil
}

func TestUploadSnapshot(t *testing.T) {
	// 3.5 blocks: data, zeros, data, then a partial final block.
	img := make([]byte, 3*BlockSize+BlockSize/2)
	copy(img, "first block")
	copy(img[2*BlockSize:], "third block")
	copy(img[3*BlockSize:], "tail")
	path := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeEBS{}
	id, err := UploadSnapshot(context.Background(), f, path, SnapshotOptions{Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if id != "snap-fake" || f.size != 1 {
		t.Errorf("id %s size %d GiB", id, f.size)
	}
	if len(f.blocks) != 3 || f.blocks[1] != nil {
		t.Fatalf("wrote blocks %v; want 0, 2 and 3 (block 1 is all zeros)", keys(f.blocks))
	}
	if !bytes.HasPrefix(f.blocks[3], []byte("tail")) || len(f.blocks[3]) != BlockSize {
		t.Error("final block not zero-padded to full size")
	}
	agg := sha256.New()
	for _, i := range []int32{0, 2, 3} {
		s := sha256.Sum256(f.blocks[i])
		agg.Write(s[:])
	}
	if got, want := aws.ToString(f.complete.Checksum), base64.StdEncoding.EncodeToString(agg.Sum(nil)); got != want {
		t.Errorf("aggregate checksum %s, want %s", got, want)
	}
	if aws.ToInt32(f.complete.ChangedBlocksCount) != 3 {
		t.Errorf("ChangedBlocksCount = %d", aws.ToInt32(f.complete.ChangedBlocksCount))
	}
}

func TestUploadSnapshotTooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(2 << 30) // sparse
	f.Close()
	if _, err := UploadSnapshot(context.Background(), &fakeEBS{}, path, SnapshotOptions{VolumeSizeGiB: 1}); err == nil {
		t.Fatal("expected error for a 2 GiB image on a 1 GiB volume")
	}
}

func keys(m map[int32][]byte) []int32 {
	var out []int32
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRegisterImageInput(t *testing.T) {
	in, err := RegisterImageInput("snap-1", 1, AMIOptions{Name: "hello", Tags: map[string]string{"b": "2", "a": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if in.Architecture != types.ArchitectureValuesX8664 {
		t.Errorf("architecture %s", in.Architecture)
	}
	if !aws.ToBool(in.EnaSupport) || in.BootMode != types.BootModeValuesUefiPreferred || in.ImdsSupport != types.ImdsSupportValuesV20 {
		t.Errorf("flags: ena=%v boot=%s imds=%s", aws.ToBool(in.EnaSupport), in.BootMode, in.ImdsSupport)
	}
	bdm := in.BlockDeviceMappings[0]
	if aws.ToString(bdm.DeviceName) != aws.ToString(in.RootDeviceName) || aws.ToString(bdm.Ebs.SnapshotId) != "snap-1" || bdm.Ebs.VolumeType != types.VolumeTypeGp3 {
		t.Errorf("block device mapping %+v", bdm)
	}
	if tags := in.TagSpecifications[0].Tags; aws.ToString(tags[0].Key) != "a" {
		t.Errorf("tags not sorted: %v", tags)
	}
}

func TestRegisterImageInputArm64(t *testing.T) {
	in, err := RegisterImageInput("snap-1", 1, AMIOptions{Name: "hello", Arch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Architecture != types.ArchitectureValuesArm64 || in.BootMode != types.BootModeValuesUefi {
		t.Errorf("architecture %s, boot mode %s", in.Architecture, in.BootMode)
	}
	if _, err := RegisterImageInput("snap-1", 1, AMIOptions{Arch: "mips"}); err == nil {
		t.Error("expected error for an unsupported architecture")
	}
}

// fakeEC2 models just enough of EC2 for Smoke: a default VPC, security
// groups, and one instance whose console prints the ready marker.
type fakeEC2 struct {
	SmokeAPI // unimplemented methods panic
	mu       sync.Mutex
	sgs      map[string]bool
	state    types.InstanceStateName
	ip       string
	console  string
	calls    []string
}

func (f *fakeEC2) log(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }

func (f *fakeEC2) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{VpcId: aws.String("vpc-1")}}}, nil
}

func (f *fakeEC2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.log("CreateSecurityGroup")
	f.sgs = map[string]bool{"sg-1": true}
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-1")}, nil
}

func (f *fakeEC2) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.log("Authorize " + aws.ToString(in.IpPermissions[0].IpRanges[0].CidrIp))
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakeEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.log("DeleteSecurityGroup")
	if f.state != types.InstanceStateNameTerminated && f.state != "" {
		return nil, fmt.Errorf("DependencyViolation")
	}
	delete(f.sgs, aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.log("RunInstances " + aws.ToString(in.ImageId))
	f.state = types.InstanceStateNameRunning
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-1")}}}, nil
}

func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{
		InstanceId:      aws.String("i-1"),
		State:           &types.InstanceState{Name: f.state},
		PublicIpAddress: aws.String(f.ip),
	}}}}}, nil
}

func (f *fakeEC2) GetConsoleOutput(context.Context, *ec2.GetConsoleOutputInput, ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error) {
	return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(f.console)))}, nil
}

func (f *fakeEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.log("TerminateInstances")
	f.state = types.InstanceStateNameTerminated
	return &ec2.TerminateInstancesOutput{}, nil
}

func TestSmoke(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from netbsd/amd64\n")
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	f := &fakeEC2{ip: "127.0.0.1", console: "...\nnbinit: ready\n"}
	var console bytes.Buffer
	err = Smoke(context.Background(), f, SmokeOptions{
		ImageID:     "ami-1",
		ReadyMarker: "nbinit: ready",
		HTTPPort:    port,
		HTTPPath:    "/",
		HTTPExpect:  "hello from netbsd",
		ProbeCIDR:   "198.51.100.1/32",
		Timeout:     time.Minute,
		Console:     &console,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "CreateSecurityGroup,Authorize 198.51.100.1/32,RunInstances ami-1,TerminateInstances,DeleteSecurityGroup"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("calls\n got %s\nwant %s", got, want)
	}
	if len(f.sgs) != 0 {
		t.Error("security group not deleted")
	}
	if !strings.Contains(console.String(), "nbinit: ready") {
		t.Error("console output not captured")
	}
}

func TestSmokeCleansUpOnFailure(t *testing.T) {
	f := &fakeEC2{ip: "127.0.0.1", console: "panic: something\n"}
	err := Smoke(context.Background(), f, SmokeOptions{ImageID: "ami-1", ReadyMarker: "nbinit: ready", Timeout: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "not seen") {
		t.Fatalf("err = %v", err)
	}
	if f.state != types.InstanceStateNameTerminated || len(f.sgs) != 0 {
		t.Errorf("left behind: state %s, groups %v", f.state, f.sgs)
	}
}
