package repository

import (
	"testing"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestParseS3Target(t *testing.T) {
	tests := []struct {
		name           string
		uri            string
		bucket, prefix string
		wantErr        bool
	}{
		{name: "bucket and prefix", uri: "s3://my-bucket/aiha/prod", bucket: "my-bucket", prefix: "aiha/prod"},
		{name: "bucket only", uri: "s3://my-bucket", bucket: "my-bucket", prefix: ""},
		{name: "trailing slash", uri: "s3://my-bucket/aiha/", bucket: "my-bucket", prefix: "aiha"},
		{name: "leading slash prefix", uri: "s3://my-bucket/a/b/c", bucket: "my-bucket", prefix: "a/b/c"},
		{name: "missing bucket", uri: "s3:///prefix", wantErr: true},
		{name: "wrong scheme", uri: "smb://host/share", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bucket, prefix, err := parseS3Target(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if bucket != tt.bucket || prefix != tt.prefix {
				t.Fatalf("got (%q,%q), want (%q,%q)", bucket, prefix, tt.bucket, tt.prefix)
			}
		})
	}
}

// TestS3CredentialsStatic verifies that static keys are used when provided.
func TestS3CredentialsStatic(t *testing.T) {
	creds := s3Credentials(S3Auth{AccessKey: "AKIA", SecretKey: "secret", SessionToken: "tok"})
	v, err := creds.GetWithContext(&credentials.CredContext{})
	if err != nil {
		t.Fatalf("get static credentials: %v", err)
	}
	if v.AccessKeyID != "AKIA" || v.SecretAccessKey != "secret" || v.SessionToken != "tok" {
		t.Fatalf("static credentials not honored: %+v", v)
	}
}

// TestS3CredentialsChain verifies a non-nil chain is returned when no static
// keys are set (the AWS default chain: env, shared file, IAM).
func TestS3CredentialsChain(t *testing.T) {
	if got := s3Credentials(S3Auth{}); got == nil {
		t.Fatal("expected a credential chain, got nil")
	}
}
