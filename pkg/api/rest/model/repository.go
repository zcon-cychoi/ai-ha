package model

import "time"

// RepositoryInfo describes a registered backup repository without its
// credentials. It is what listing and inspecting a repository return;
// RepositoryAccess, which carries the credentials, goes only to an agent
// carrying out a job against the repository.
type RepositoryInfo struct {
	ID       string `json:"id"                 example:"3f2a9c1e6b7d4a80"`
	Name     string `json:"name"               example:"minio-main"`
	Type     string `json:"type"               example:"s3"`
	Provider string `json:"provider"           example:"Minio"`
	Endpoint string `json:"endpoint,omitempty" example:"https://storage.example.com:9000"`
	Region   string `json:"region,omitempty"   example:"us-east-1"`
	Bucket   string `json:"bucket"             example:"aiha-backup"`
	Prefix   string `json:"prefix,omitempty"   example:"prod"`
	// HasSessionToken says whether the credentials include a session token,
	// which expires and is not renewed by the framework.
	HasSessionToken      bool      `json:"hasSessionToken"`
	CreatedAt            time.Time `json:"createdAt"`
	CredentialsUpdatedAt time.Time `json:"credentialsUpdatedAt"`
}
