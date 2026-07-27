package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3ObjectClient interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3Backend struct {
	bucket string
	client s3ObjectClient
}

func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("S3_ENDPOINT is required for s3 storage")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3_BUCKET is required for s3 storage")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("S3_REGION is required for s3 storage")
	}

	httpClient, err := objectStorageHTTPClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
		awsconfig.WithHTTPClient(httpClient),
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.ForcePathStyle
		options.BaseEndpoint = aws.String(cfg.Endpoint)
	})
	return &S3Backend{bucket: cfg.Bucket, client: client}, nil
}

func (b *S3Backend) Put(ctx context.Context, key string, reader io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("put S3 object %q: size must be non-negative", key)
	}
	body, cleanup, err := seekableBody(reader, size)
	if err != nil {
		return fmt.Errorf("prepare S3 object %q: %w", key, err)
	}
	defer cleanup()

	_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("put S3 object %q: %w", key, err)
	}
	return nil
}

func (b *S3Backend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get S3 object %q: %w", key, err)
	}
	size := int64(0)
	if output.ContentLength != nil {
		size = *output.ContentLength
	}
	return output.Body, size, nil
}

func seekableBody(reader io.Reader, size int64) (io.ReadSeeker, func(), error) {
	if seekable, ok := reader.(io.ReadSeeker); ok {
		return seekable, func() {}, nil
	}

	tempFile, err := os.CreateTemp("", "costrict-s3-put-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary upload file: %w", err)
	}
	cleanup := func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}
	written, err := io.Copy(tempFile, reader)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("buffer upload: %w", err)
	}
	if written != size {
		cleanup()
		return nil, nil, fmt.Errorf("upload size mismatch: read %d bytes, expected %d", written, size)
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("rewind buffered upload: %w", err)
	}
	return tempFile, cleanup, nil
}

func httpClientWithCA(caFile string) (*http.Client, error) {
	return objectStorageHTTPClient(caFile)
}

func objectStorageHTTPClient(caFile string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default HTTP transport has unexpected type")
	}
	customTransport := transport.Clone()
	customTransport.ResponseHeaderTimeout = 30 * time.Second
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read S3 CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA pool: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("S3 CA file contains no valid certificates")
		}
		tlsConfig := &tls.Config{}
		if customTransport.TLSClientConfig != nil {
			tlsConfig = customTransport.TLSClientConfig.Clone()
		}
		tlsConfig.RootCAs = roots
		customTransport.TLSClientConfig = tlsConfig
	}
	return &http.Client{Transport: customTransport}, nil
}
