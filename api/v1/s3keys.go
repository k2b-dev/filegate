package v1

// Types for the S3 access-key endpoints.
//
// Access keys are resources, not configuration: they have identity, need
// rotation, and every change belongs in an audit log. They therefore get real
// CRUD instead of being edited as a list inside a config value.

// S3Key is one access key. The secret is never included; it is returned only
// once, at creation or rotation, because it cannot be recovered afterwards.
type S3Key struct {
	AccessKey string   `json:"accessKey"`
	Buckets   []string `json:"buckets"`
	// RequestsPerSecond and Burst are the per-key rate limit; zero means none.
	RequestsPerSecond int `json:"requestsPerSecond,omitempty"`
	Burst             int `json:"burst,omitempty"`
	// Disabled keys stay stored so they can be re-enabled, but do not
	// authenticate.
	Disabled  bool  `json:"disabled"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// S3KeyCreated carries the one-time secret alongside the key.
type S3KeyCreated struct {
	S3Key
	// SecretKey is shown exactly once. Store it now or rotate the key.
	SecretKey string `json:"secretKey"`
}

// S3KeyListResponse is the body of GET /v1/s3/keys.
type S3KeyListResponse struct {
	Items []S3Key `json:"items"`
	Total int     `json:"total"`
}

// S3KeyCreateRequest creates a key. An empty accessKey is generated.
type S3KeyCreateRequest struct {
	AccessKey string `json:"accessKey,omitempty"`
	// Buckets is required. Use ["*"] to grant every mount.
	Buckets           []string `json:"buckets"`
	RequestsPerSecond int      `json:"requestsPerSecond,omitempty"`
	Burst             int      `json:"burst,omitempty"`
}

// S3KeyUpdateRequest changes grants, limits or the disabled flag. Nil fields
// are left untouched, which is why they are pointers.
type S3KeyUpdateRequest struct {
	Buckets           *[]string `json:"buckets,omitempty"`
	RequestsPerSecond *int      `json:"requestsPerSecond,omitempty"`
	Burst             *int      `json:"burst,omitempty"`
	Disabled          *bool     `json:"disabled,omitempty"`
}
