// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestCloudMapReport(t *testing.T) {
	var got map[string]string
	var target, auth string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, auth = r.Header.Get("X-Amz-Target"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"__type":"CustomHealthNotFound","Message":"no custom health check"}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "token")
	c := NewCloudMap("ap-northeast-1", "srv-1", "i-0abc", srv.URL, creds)
	if err := c.Report(context.Background(), Unhealthy); err != nil {
		t.Fatal(err)
	}
	if target != "Route53AutoNaming_v20170314.UpdateInstanceCustomHealthStatus" {
		t.Errorf("X-Amz-Target = %q", target)
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/ap-northeast-1/servicediscovery/aws4_request") {
		t.Errorf("Authorization = %q", auth)
	}
	want := map[string]string{"ServiceId": "srv-1", "InstanceId": "i-0abc", "Status": "UNHEALTHY"}
	if len(got) != len(want) || got["ServiceId"] != want["ServiceId"] || got["InstanceId"] != want["InstanceId"] || got["Status"] != want["Status"] {
		t.Errorf("body = %v, want %v", got, want)
	}

	fail = true
	if err := c.Report(context.Background(), Healthy); err == nil || !strings.Contains(err.Error(), "CustomHealthNotFound") {
		t.Errorf("err = %v", err)
	}
}
