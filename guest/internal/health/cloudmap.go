// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
)

// CloudMap reports with servicediscovery:UpdateInstanceCustomHealthStatus,
// which needs the service to have a custom health check configuration.
type CloudMap struct {
	ServiceID, InstanceID string
	client                *servicediscovery.Client
}

// NewCloudMap signs requests with the EC2 instance profile's credentials,
// read from IMDSv2 and refreshed before they expire. endpoint overrides the
// regional endpoint; it is for tests and is normally empty.
func NewCloudMap(region, serviceID, instanceID, endpoint string, creds aws.CredentialsProvider) *CloudMap {
	if creds == nil {
		creds = aws.NewCredentialsCache(ec2rolecreds.New())
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: creds,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
	return &CloudMap{
		ServiceID:  serviceID,
		InstanceID: instanceID,
		client: servicediscovery.NewFromConfig(cfg, func(o *servicediscovery.Options) {
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
		}),
	}
}

func (c *CloudMap) Report(ctx context.Context, s Status) error {
	_, err := c.client.UpdateInstanceCustomHealthStatus(ctx, &servicediscovery.UpdateInstanceCustomHealthStatusInput{
		ServiceId:  aws.String(c.ServiceID),
		InstanceId: aws.String(c.InstanceID),
		Status:     types.CustomHealthStatus(s),
	})
	return err
}
