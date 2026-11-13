// Package recordings keeps call recordings and voicemails in the
// installation's own bucket instead of at SignalWire, and deletes them a
// year after the call.
package recordings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Object is a recording read back from the bucket.
type Object struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
	// ContentRange is set when only part of the file was asked for.
	ContentRange string
}

// Store is where recordings are kept.
type Store interface {
	// Put stores a file and returns its version, which Delete needs to
	// remove it for good.
	Put(ctx context.Context, key, contentType string, data []byte) (version string, err error)
	Get(ctx context.Context, key, byteRange string) (Object, error)
	Delete(ctx context.Context, key, version string) error
}

// S3 is a Store on an S3-compatible bucket, such as Backblaze B2.
type S3 struct {
	Bucket string
	client *s3.Client
}

// NewS3 connects to a bucket. B2 is versioned, so a delete without a version
// would only hide a file; Delete always names the version.
func NewS3(endpoint, region, bucket, keyID, secret string) (*S3, error) {
	if !strings.HasPrefix(endpoint, "https://") || bucket == "" || keyID == "" || secret == "" {
		return nil, errors.New("recordings storage needs an https endpoint, a bucket and a key")
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(keyID, secret, ""),
		UsePathStyle: true,
		// B2 does not take the SDK's default checksum headers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3{Bucket: bucket, client: client}, nil
}

func (s *S3) Put(ctx context.Context, key, contentType string, data []byte) (string, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket), Key: aws.String(key), ContentType: aws.String(contentType),
		Body: strings.NewReader(string(data)), ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.VersionId), nil
}

func (s *S3) Get(ctx context.Context, key, byteRange string) (Object, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)}
	if byteRange != "" {
		in.Range = aws.String(byteRange)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return Object{}, err
	}
	return Object{Body: out.Body, ContentType: aws.ToString(out.ContentType), ContentLength: aws.ToInt64(out.ContentLength), ContentRange: aws.ToString(out.ContentRange)}, nil
}

func (s *S3) Delete(ctx context.Context, key, version string) error {
	in := &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	_, err := s.client.DeleteObject(ctx, in)
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NoSuchVersion") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	return nil
}

// Status is the HTTP status for serving an object.
func (o Object) Status() int {
	if o.ContentRange != "" {
		return http.StatusPartialContent
	}
	return http.StatusOK
}
