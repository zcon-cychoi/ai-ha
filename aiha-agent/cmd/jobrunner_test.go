//go:build !rescue

package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	apimodel "github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
)

func TestS3Target(t *testing.T) {
	base := apimodel.RepositoryAccess{Type: "s3", Bucket: "aiha-backup", Prefix: "prod", Region: "us-east-1",
		AccessKey: "a", SecretKey: "s", SessionToken: "t"}
	tests := []struct {
		name     string
		endpoint string
		prefix   string
		wantURI  string
		wantAuth repository.S3Auth
	}{
		{"minio over http", "http://minio.example.com:19000", "prod", "s3://aiha-backup/prod",
			repository.S3Auth{Endpoint: "minio.example.com:19000", UseSSL: false}},
		{"minio over https without a port", "https://minio.example.com", "prod", "s3://aiha-backup/prod",
			repository.S3Auth{Endpoint: "minio.example.com", UseSSL: true}},
		{"aws at the bucket root", "", "", "s3://aiha-backup",
			repository.S3Auth{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := base
			a.Endpoint, a.Prefix = tt.endpoint, tt.prefix
			uri, auth, err := s3Target(a)
			if err != nil {
				t.Fatalf("s3Target: %v", err)
			}
			want := tt.wantAuth
			want.AccessKey, want.SecretKey, want.SessionToken, want.Region = "a", "s", "t", "us-east-1"
			if uri != tt.wantURI || auth != want {
				t.Fatalf("got %s %+v, want %s %+v", uri, auth, tt.wantURI, want)
			}
		})
	}

	for name, a := range map[string]apimodel.RepositoryAccess{
		"none":         {},
		"not s3":       {Type: "sftp", Bucket: "b"},
		"no bucket":    {Type: "s3"},
		"bad endpoint": {Type: "s3", Bucket: "b", Endpoint: "minio.example.com:9000"},
		"ftp endpoint": {Type: "s3", Bucket: "b", Endpoint: "ftp://minio.example.com"},
	} {
		if _, _, err := s3Target(a); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// A job that arrives without a repository fails before anything is read; it
// does not fall back to the repository configured on this host.
func TestBackupRunnerRefusesAJobWithoutARepository(t *testing.T) {
	spec, _ := json.Marshal(apimodel.BackupJobSpec{Devices: []string{"/dev/sda1"}, RepositoryID: "0123456789abcdef"})
	var progress atomic.Int64
	_, err := backupRunner{}.Run(context.Background(), "job-1", spec, apimodel.RepositoryAccess{}, &progress)
	if err == nil || !strings.Contains(err.Error(), "no repository") {
		t.Fatalf("err = %v, want a missing-repository refusal", err)
	}
}
