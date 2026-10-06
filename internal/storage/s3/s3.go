// Package s3 stores objects in an S3 bucket or any S3-compatible store
// (MinIO, Cloudflare R2, ...).
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

type Options struct {
	Bucket string
	Region string
	// Endpoint overrides the AWS endpoint (MinIO, R2). Empty = AWS.
	Endpoint string
	// PathStyle addresses objects as endpoint/bucket/key instead of
	// bucket.endpoint/key; most self-hosted stores need it.
	PathStyle bool
	// Prefix is prepended to every key, so several services can share a bucket.
	Prefix string
	// CreateBucket creates Bucket if it does not exist yet.
	CreateBucket bool
}

type Store struct {
	client   *s3.Client
	uploader *transfermanager.Client
	presign  *s3.PresignClient
	bucket   string
	prefix   string
}

var (
	_ storage.Storage   = (*Store)(nil)
	_ storage.URLSigner = (*Store)(nil)
)

// New builds a client from the standard AWS credential chain plus opts.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3 storage: bucket is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(opts.Region))
	if err != nil {
		return nil, fmt.Errorf("s3 storage: load AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.PathStyle
	})
	if opts.CreateBucket {
		if err := ensureBucket(ctx, client, opts.Bucket); err != nil {
			return nil, err
		}
	}
	return &Store{
		client:   client,
		uploader: transfermanager.New(client),
		presign:  s3.NewPresignClient(client),
		bucket:   opts.Bucket,
		prefix:   strings.Trim(opts.Prefix, "/"),
	}, nil
}

// ensureBucket creates bucket unless it exists. A concurrent creator (another
// instance starting at the same time) is not an error.
func ensureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || (apiErr.ErrorCode() != "NotFound" && apiErr.ErrorCode() != "NoSuchBucket") {
		return fmt.Errorf("s3 storage: check bucket %q: %w", bucket, err)
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "BucketAlreadyOwnedByYou" || apiErr.ErrorCode() == "BucketAlreadyExists") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("s3 storage: create bucket %q: %w", bucket, err)
	}
	return nil
}

func (s *Store) objectKey(key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	if s.prefix == "" {
		return key, nil
	}
	return s.prefix + "/" + key, nil
}

// Put streams r through a multipart upload, so the body length need not be
// known up front. S3 only makes an object visible once the upload completes;
// a failed upload is aborted by the transfer manager and leaves nothing behind.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	ok, err := s.objectKey(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	in := &transfermanager.UploadObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(ok),
		Body:     r,
		Metadata: encodeMeta(info.Meta),
	}
	if info.ContentType != "" {
		in.ContentType = aws.String(info.ContentType)
	}
	if _, err := s.uploader.UploadObject(ctx, in); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3 put %s: %w", key, err)
	}
	return s.Stat(ctx, key)
}

func (s *Store) Open(ctx context.Context, key string, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	ok, err := s.objectKey(key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ok)}
	if rng != nil {
		if rng.Start < 0 || (rng.End >= 0 && rng.End < rng.Start) {
			return nil, storage.ObjectInfo{}, fmt.Errorf("%w: %d-%d", storage.ErrInvalidRange, rng.Start, rng.End)
		}
		h := fmt.Sprintf("bytes=%d-", rng.Start)
		if rng.End >= 0 {
			h += strconv.FormatInt(rng.End, 10)
		}
		in.Range = aws.String(h)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapErr(key, err)
	}

	info := storage.ObjectInfo{
		Key:         key,
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
		ModTime:     aws.ToTime(out.LastModified),
		Meta:        decodeMeta(out.Metadata),
	}
	// For a ranged GET, ContentLength is the slice; the full size is the
	// "/total" part of Content-Range.
	if rng != nil {
		if total, ok := totalFromContentRange(aws.ToString(out.ContentRange)); ok {
			info.Size = total
		}
	}
	return out.Body, info, nil
}

func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	ok, err := s.objectKey(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ok)})
	if err != nil {
		return storage.ObjectInfo{}, mapErr(key, err)
	}
	return storage.ObjectInfo{
		Key:         key,
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
		ModTime:     aws.ToTime(out.LastModified),
		Meta:        decodeMeta(out.Metadata),
	}, nil
}

// Delete reports ErrNotFound for a missing key, matching the local backend;
// S3's own DeleteObject succeeds silently in that case.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.Stat(ctx, key); err != nil {
		return err
	}
	ok, _ := s.objectKey(key) // validated by Stat
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ok)}); err != nil {
		return mapErr(key, err)
	}
	return nil
}

func (s *Store) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	ok, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	req, err := s.presign.PresignGetObject(ctx,
		&s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ok)},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3 presign %s: %w", key, err)
	}
	return req.URL, nil
}

func mapErr(key string, err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return fmt.Errorf("%w: %s", storage.ErrNotFound, key)
		case "InvalidRange":
			return fmt.Errorf("%w: %s", storage.ErrInvalidRange, key)
		}
	}
	return fmt.Errorf("s3 %s: %w", key, err)
}

// S3 user metadata travels as HTTP headers, which only carry ASCII safely;
// values are query-escaped on the way in so titles in any script round-trip.
func encodeMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = url.QueryEscape(v)
	}
	return out
}

func decodeMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		out[k] = v
	}
	return out
}

// totalFromContentRange parses "bytes 0-99/1234" → 1234.
func totalFromContentRange(h string) (int64, bool) {
	i := strings.LastIndexByte(h, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(h[i+1:], 10, 64)
	return n, err == nil
}
