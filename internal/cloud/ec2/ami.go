// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// RootDevice is the device name the AMI's root volume is mapped to. The
// name is only a label on Nitro, where every EBS volume is an NVMe device.
const RootDevice = "/dev/xvda"

// AMIOptions describes the AMI to register.
type AMIOptions struct {
	Name        string
	Description string
	Tags        map[string]string
	// Arch is the image's NetBSD architecture: "amd64" (default) or
	// "aarch64" (Graviton).
	Arch string
	// BootMode is "uefi-preferred" (the amd64 default), "uefi" (the only
	// choice, and default, for aarch64) or "legacy-bios". amd64 images boot
	// either way; uefi-preferred lets EC2 pick per instance type.
	BootMode string
}

// ec2Arch maps NetBSD architecture names to EC2's.
func ec2Arch(arch string) (types.ArchitectureValues, error) {
	switch arch {
	case "", "amd64":
		return types.ArchitectureValuesX8664, nil
	case "aarch64":
		return types.ArchitectureValuesArm64, nil
	}
	return "", fmt.Errorf("unsupported architecture %q", arch)
}

func ec2Tags(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []types.Tag
	for _, k := range keys {
		out = append(out, types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// WaitSnapshot waits until EC2 reports the snapshot as completed.
func WaitSnapshot(ctx context.Context, api ec2.DescribeSnapshotsAPIClient, id string, timeout time.Duration) error {
	w := ec2.NewSnapshotCompletedWaiter(api, func(o *ec2.SnapshotCompletedWaiterOptions) {
		o.MinDelay = 2 * time.Second
		o.MaxDelay = 15 * time.Second
	})
	return w.Wait(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{id}}, timeout)
}

// RegisterImageInput builds the RegisterImage request for a snapshot made by
// UploadSnapshot. It is separate from Register so that tests can check it.
func RegisterImageInput(snapshotID string, volumeGiB int32, opt AMIOptions) (*ec2.RegisterImageInput, error) {
	arch, err := ec2Arch(opt.Arch)
	if err != nil {
		return nil, err
	}
	mode := opt.BootMode
	if mode == "" {
		mode = "uefi-preferred"
		if arch == types.ArchitectureValuesArm64 {
			mode = "uefi" // arm64 images carry no BIOS boot code
		}
	}
	in := &ec2.RegisterImageInput{
		Name:               aws.String(opt.Name),
		Architecture:       arch,
		VirtualizationType: aws.String("hvm"),
		RootDeviceName:     aws.String(RootDevice),
		// The CLOUD kernel has ena(4) and nvme(4); EC2 requires the flag
		// for Nitro instance types.
		EnaSupport:  aws.Bool(true),
		BootMode:    types.BootModeValues(mode),
		ImdsSupport: types.ImdsSupportValuesV20, // the init shim speaks IMDSv2
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String(RootDevice),
			Ebs: &types.EbsBlockDevice{
				SnapshotId:          aws.String(snapshotID),
				VolumeSize:          aws.Int32(volumeGiB),
				VolumeType:          types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true),
			},
		}},
	}
	if opt.Description != "" {
		in.Description = aws.String(opt.Description)
	}
	if len(opt.Tags) > 0 {
		in.TagSpecifications = []types.TagSpecification{{ResourceType: types.ResourceTypeImage, Tags: ec2Tags(opt.Tags)}}
	}
	return in, nil
}

// EC2API is the subset of the EC2 client used for publishing.
type EC2API interface {
	ec2.DescribeSnapshotsAPIClient
	ec2.DescribeImagesAPIClient
	RegisterImage(context.Context, *ec2.RegisterImageInput, ...func(*ec2.Options)) (*ec2.RegisterImageOutput, error)
	DeregisterImage(context.Context, *ec2.DeregisterImageInput, ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error)
	DeleteSnapshot(context.Context, *ec2.DeleteSnapshotInput, ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
}

// Published identifies an AMI and the snapshot behind it.
type Published struct {
	Region     string `json:"region"`
	ImageID    string `json:"image_id"`
	SnapshotID string `json:"snapshot_id"`
	Name       string `json:"name"`
}

// Publish uploads the image, registers an AMI and waits until it is
// available.
func Publish(ctx context.Context, ebsAPI EBSAPI, ec2API EC2API, region, image string, snap SnapshotOptions, ami AMIOptions, logf func(string, ...any)) (*Published, error) {
	if _, err := ec2Arch(ami.Arch); err != nil {
		return nil, err
	}
	start := time.Now()
	snapID, err := UploadSnapshot(ctx, ebsAPI, image, snap)
	if err != nil {
		return nil, err
	}
	logf("uploaded %s in %v; waiting for it to complete", snapID, time.Since(start).Round(time.Second))
	if err := WaitSnapshot(ctx, ec2API, snapID, 30*time.Minute); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", snapID, err)
	}
	size, err := snapshotSize(ctx, ec2API, snapID)
	if err != nil {
		return nil, err
	}
	in, err := RegisterImageInput(snapID, size, ami)
	if err != nil {
		return nil, err
	}
	out, err := ec2API.RegisterImage(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("RegisterImage: %w (snapshot %s is left behind)", err, snapID)
	}
	p := &Published{Region: region, ImageID: aws.ToString(out.ImageId), SnapshotID: snapID, Name: ami.Name}
	w := ec2.NewImageAvailableWaiter(ec2API, func(o *ec2.ImageAvailableWaiterOptions) {
		o.MinDelay = 2 * time.Second
		o.MaxDelay = 15 * time.Second
	})
	if err := w.Wait(ctx, &ec2.DescribeImagesInput{ImageIds: []string{p.ImageID}}, 30*time.Minute); err != nil {
		return p, fmt.Errorf("image %s: %w", p.ImageID, err)
	}
	logf("registered %s (%s) in %v", p.ImageID, p.Name, time.Since(start).Round(time.Second))
	return p, nil
}

func snapshotSize(ctx context.Context, api ec2.DescribeSnapshotsAPIClient, id string) (int32, error) {
	out, err := api.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{id}})
	if err != nil {
		return 0, err
	}
	if len(out.Snapshots) != 1 {
		return 0, fmt.Errorf("snapshot %s not found", id)
	}
	return aws.ToInt32(out.Snapshots[0].VolumeSize), nil
}

// Unpublish deregisters the AMI and deletes its snapshot.
func Unpublish(ctx context.Context, api EC2API, p *Published) error {
	if p.ImageID != "" {
		if _, err := api.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(p.ImageID)}); err != nil {
			return fmt.Errorf("DeregisterImage %s: %w", p.ImageID, err)
		}
	}
	if p.SnapshotID != "" {
		if _, err := api.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(p.SnapshotID)}); err != nil {
			return fmt.Errorf("DeleteSnapshot %s: %w", p.SnapshotID, err)
		}
	}
	return nil
}
