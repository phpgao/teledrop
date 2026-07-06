package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Uploader uploads to an S3-compatible object store (AWS S3 / Tencent COS / MinIO / ...).
type S3Uploader struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Uploader builds the S3/COS uploader. When endpoint is empty, AWS default resolution applies.
// A 30-second timeout guards connect+credential validation.
func NewS3Uploader(cfg S3Config) (*S3Uploader, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("uploader: s3 uploader missing bucket (set upload.s3.bucket in config.yaml)")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("uploader: s3 uploader missing access_key/secret_key (set upload.s3.access_key and upload.s3.secret_key in config.yaml)")
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	// Auto-prefix https:// when the user omitted it (common COS/MinIO mistake).
	endpoint := cfg.Endpoint
	if endpoint != "" && !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("uploader: load aws config failed (check region/credentials/network): %w", err)
	}

	clientOpts := func(o *s3.Options) {
		o.UsePathStyle = cfg.UsePathStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	}
	client := s3.NewFromConfig(awsCfg, clientOpts)

	// Optional connectivity check — warns on failure but does not block startup.
	if cfg.HealthCheck {
		hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer hcancel()
		if _, err := client.HeadBucket(hctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
			log.Printf("warn: s3 health check failed (bucket=%s, endpoint=%s): %v", cfg.Bucket, endpoint, err)
		}
	}

	return &S3Uploader{client: client, bucket: cfg.Bucket, prefix: strings.Trim(cfg.Prefix, "/")}, nil
}

// fullKey joins the configured prefix with the relative key.
func (u *S3Uploader) fullKey(key string) string {
	if u.prefix == "" {
		return key
	}
	return u.prefix + "/" + strings.TrimLeft(key, "/")
}

// Upload uploads the local file to bucket/fullKey. When overwrite=false and the object
// exists, returns ErrExists.
func (u *S3Uploader) Upload(ctx context.Context, src, key string, overwrite bool) error {
	fk := u.fullKey(key)

	if !overwrite {
		_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(u.bucket),
			Key:    aws.String(fk),
		})
		if err == nil {
			return ErrExists
		}
		var nsk *types.NotFound
		if !errors.As(err, &nsk) {
			return fmt.Errorf("uploader: check object existence failed: %w", err)
		}
	}

	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("uploader: open source failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(fk),
		Body:   f,
	}); err != nil {
		return fmt.Errorf("uploader: upload object failed: %w", err)
	}
	return nil
}
