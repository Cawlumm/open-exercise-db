package media

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// r2Region is the region every R2 bucket reports over the S3 API. R2 has no
// user-selectable regions the way S3 does — Cloudflare documents "auto" as
// the value, aliasing an empty string and "us-east-1" to it for tools that
// insist on a real region name
// (https://developers.cloudflare.com/r2/api/s3/api/ → "Bucket region").
// SigV4 still needs *some* region in the signing scope, so this is not
// optional.
const r2Region = "auto"

// R2Store implements Store against Cloudflare R2 over its S3-compatible
// API. Production's backend; FilesystemStore covers local development and
// self-hosters with no object storage (docs/DESIGN.md → Hosting).
//
// Only Put and Get are implemented, because Store only declares those two —
// this deliberately does not grow presigned uploads, listing, or deletion
// until something needs them (Phase 2's contributor uploads is the likely
// first caller for presigning).
type R2Store struct {
	client *s3.Client
	bucket string
}

// NewR2Store builds a Store backed by the R2 bucket named bucket in the
// account identified by accountID.
//
// The S3 client is constructed via s3.New with an explicit s3.Options
// rather than the usual config.LoadDefaultConfig + s3.NewFromConfig pair
// that Cloudflare's own examples show. Two reasons, both about what
// LoadDefaultConfig does that we do not want:
//
//   - It walks the full AWS credential chain — environment variables,
//     ~/.aws/credentials, SSO, STS role assumption, and finally the EC2
//     instance-metadata service. Every one of those is a way for this
//     process to silently authenticate as something other than the R2 token
//     it was configured with, and the IMDS probe in particular blocks on a
//     network timeout when no metadata endpoint exists, which is every
//     environment we deploy to.
//   - It pulls github.com/aws/aws-sdk-go-v2/config and its transitive sso /
//     ssooidc / sts / imds dependencies into the build for machinery this
//     code never uses. Static credentials are the only mode R2 supports.
//
// Passing credentials explicitly makes "which key is this process using?"
// answerable from config alone, with no ambient environment to account for.
func NewR2Store(accountID, accessKeyID, secretAccessKey, bucket string) (*R2Store, error) {
	switch {
	case accountID == "":
		return nil, errors.New("r2: account id is required")
	case accessKeyID == "":
		return nil, errors.New("r2: access key id is required")
	case secretAccessKey == "":
		return nil, errors.New("r2: secret access key is required")
	case bucket == "":
		return nil, errors.New("r2: bucket is required")
	}

	client := s3.New(s3.Options{
		Region:       r2Region,
		BaseEndpoint: aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)),
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKey, "",
		),
	})

	return &R2Store{client: client, bucket: bucket}, nil
}

// Put uploads r to key, recording contentType as the object's Content-Type
// metadata.
//
// FilesystemStore deliberately drops contentType (exercise_images.content_type
// is the source of truth, not the storage layer) — this does not, because on
// R2 the field is free to set and is what a public bucket or CDN would serve
// in the response header. Storing it keeps the door open for Phase 2's
// IMAGE_BASE_URL pointing clients straight at R2, where nothing on our side
// is in the request path to supply that header. exercise_images.content_type
// remains authoritative for anything the API itself renders.
//
// Unlike FilesystemStore.Put there is no temp-write-then-rename dance: S3
// PutObject is already atomic per key, and a failed upload leaves no
// partial object for a reader to observe.
func (s *R2Store) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}

	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("r2 put %s: %w", key, err)
	}
	return nil
}

// Get opens key for reading. The caller owns closing the returned body —
// same contract as FilesystemStore.Get, and doubly important here since an
// unclosed S3 response body leaks an HTTP connection rather than just a
// file descriptor.
//
// A missing object is reported as ErrObjectNotFound so callers can tell it
// apart from a credential, network, or permission failure. That distinction
// matters operationally: a missing object with a live exercise_images row
// means an incomplete cmd/migrate-media backfill, whereas a 403 means the
// token is wrong — two very different pages to open at 2am, indistinguishable
// if both surface as a bare error.
func (s *R2Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, fmt.Errorf("r2 get %s: %w", key, ErrObjectNotFound)
		}
		return nil, fmt.Errorf("r2 get %s: %w", key, err)
	}
	return out.Body, nil
}

// Exists reports whether key holds an object, using HeadObject so only
// headers cross the wire — Get would answer the same question by
// downloading and billing for the entire object, and internal/seed asks
// this once per image for the whole dataset.
//
// The not-found check tests types.NotFound *and* types.NoSuchKey, which is
// not redundant: S3 returns NoSuchKey for a missing GetObject but NotFound
// for a missing HeadObject (a HEAD response has no body to carry the
// richer error code in), and S3-compatible implementations are inconsistent
// about which they mirror. Matching only one leaves a missing object
// surfacing as a transport error on some backends, which the caller is
// required to treat as "unknown, leave alone" — silently disabling repair
// on exactly the deployments that need it.
func (s *R2Store) Exists(ctx context.Context, key string) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &notFound) || errors.As(err, &noSuchKey) {
			return false, nil
		}
		return false, fmt.Errorf("r2 head %s: %w", key, err)
	}
	return true, nil
}

// ErrObjectNotFound reports that a key does not exist in the store. Both
// Store implementations wrap it for that condition — R2Store.Get on
// types.NoSuchKey, FilesystemStore.Get on os.ErrNotExist — so a caller can
// match it with errors.Is without knowing which backend it holds.
//
// Exists does not use it: "absent" is that method's ordinary answer rather
// than a failure, so it reports (false, nil) and reserves a non-nil error
// for genuinely not knowing.
//
// Declared here rather than in doc.go beside the interface because it is
// part of Get's error contract, and keeping it next to the two returns that
// produce it is what stops the two backends from drifting on it.
var ErrObjectNotFound = errors.New("object not found")
