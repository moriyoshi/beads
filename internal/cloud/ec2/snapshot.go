// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package ec2 publishes raw disk images as Amazon EC2 AMIs and smoke-tests
// them.
//
// Images are uploaded with the EBS direct APIs rather than VM Import, which
// avoids an S3 bucket and the vmimport service role, skips all-zero blocks,
// and finishes in seconds.
package ec2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	ebstypes "github.com/aws/aws-sdk-go-v2/service/ebs/types"
)

// BlockSize is the fixed EBS direct API block size.
const BlockSize = 512 << 10

const gib = 1 << 30

// EBSAPI is the subset of the EBS direct API client used here.
type EBSAPI interface {
	StartSnapshot(context.Context, *ebs.StartSnapshotInput, ...func(*ebs.Options)) (*ebs.StartSnapshotOutput, error)
	PutSnapshotBlock(context.Context, *ebs.PutSnapshotBlockInput, ...func(*ebs.Options)) (*ebs.PutSnapshotBlockOutput, error)
	CompleteSnapshot(context.Context, *ebs.CompleteSnapshotInput, ...func(*ebs.Options)) (*ebs.CompleteSnapshotOutput, error)
}

// SnapshotOptions controls UploadSnapshot.
type SnapshotOptions struct {
	Description string
	// VolumeSizeGiB is the snapshot's volume size; zero rounds the image size
	// up to whole GiB.
	VolumeSizeGiB int64
	Tags          map[string]string
	// Concurrency is the number of blocks uploaded in parallel (default 16).
	Concurrency int
	// Progress, if set, is called after each block is written.
	Progress func(done, total int)
}

func checksum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// UploadSnapshot writes the raw image at path into a new EBS snapshot and
// returns its ID. The snapshot is "completed" from the API's point of view
// when this returns, but EC2 may report it as pending for a little longer.
func UploadSnapshot(ctx context.Context, api EBSAPI, path string, opt SnapshotOptions) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := opt.VolumeSizeGiB
	if size == 0 {
		size = (st.Size() + gib - 1) / gib
	}
	if size*gib < st.Size() {
		return "", fmt.Errorf("image is %d bytes, larger than a %d GiB volume", st.Size(), size)
	}

	var tags []ebstypes.Tag
	for k, v := range opt.Tags {
		tags = append(tags, ebstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	start, err := api.StartSnapshot(ctx, &ebs.StartSnapshotInput{
		VolumeSize:  aws.Int64(size),
		Description: aws.String(opt.Description),
		Tags:        tags,
		Timeout:     aws.Int32(60), // minutes before an abandoned snapshot errors out
	})
	if err != nil {
		return "", fmt.Errorf("StartSnapshot: %w", err)
	}
	id := aws.ToString(start.SnapshotId)

	nblocks := int((st.Size() + BlockSize - 1) / BlockSize)
	sums := make([][]byte, nblocks) // nil for skipped all-zero blocks
	if err := putBlocks(ctx, api, f, id, nblocks, sums, opt); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", id, err)
	}

	// LINEAR aggregation: SHA-256 over the concatenated per-block checksums
	// of the written blocks, in block-index order.
	agg := sha256.New()
	changed := int32(0)
	for _, s := range sums {
		if s != nil {
			agg.Write(s)
			changed++
		}
	}
	_, err = api.CompleteSnapshot(ctx, &ebs.CompleteSnapshotInput{
		SnapshotId:                aws.String(id),
		ChangedBlocksCount:        aws.Int32(changed),
		Checksum:                  aws.String(base64.StdEncoding.EncodeToString(agg.Sum(nil))),
		ChecksumAlgorithm:         ebstypes.ChecksumAlgorithmChecksumAlgorithmSha256,
		ChecksumAggregationMethod: ebstypes.ChecksumAggregationMethodChecksumAggregationLinear,
	})
	if err != nil {
		return "", fmt.Errorf("CompleteSnapshot %s: %w", id, err)
	}
	return id, nil
}

func putBlocks(ctx context.Context, api EBSAPI, r io.ReaderAt, id string, nblocks int, sums [][]byte, opt SnapshotOptions) error {
	workers := opt.Concurrency
	if workers <= 0 {
		workers = 16
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var (
		mu       sync.Mutex
		firstErr error
		done     int
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	zero := make([]byte, BlockSize)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, BlockSize)
			for i := range jobs {
				// The final block is zero-padded to the full block size.
				clear(buf)
				n, err := r.ReadAt(buf, int64(i)*BlockSize)
				if err != nil && !errors.Is(err, io.EOF) {
					fail(err)
					continue
				}
				if n == 0 || bytes.Equal(buf, zero) {
					continue // unwritten blocks read back as zeros
				}
				sum := checksum(buf)
				_, err = api.PutSnapshotBlock(ctx, &ebs.PutSnapshotBlockInput{
					SnapshotId:        aws.String(id),
					BlockIndex:        aws.Int32(int32(i)),
					BlockData:         bytes.NewReader(append([]byte(nil), buf...)),
					DataLength:        aws.Int32(BlockSize),
					Checksum:          aws.String(base64.StdEncoding.EncodeToString(sum)),
					ChecksumAlgorithm: ebstypes.ChecksumAlgorithmChecksumAlgorithmSha256,
				})
				if err != nil {
					fail(fmt.Errorf("PutSnapshotBlock %d: %w", i, err))
					continue
				}
				mu.Lock()
				sums[i] = sum
				done++
				if opt.Progress != nil {
					opt.Progress(done, nblocks)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for i := 0; i < nblocks; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
