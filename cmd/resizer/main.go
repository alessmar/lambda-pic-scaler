// Command resizer is an AWS Lambda triggered by EventBridge S3 "Object Created" events. For each image it reads
// the "resize" user-metadata (JSON array of {w,h,postfix}) and writes one resized copy per entry.
package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/alessmar/lambda-pic-scaler/internal/app"
	"github.com/alessmar/lambda-pic-scaler/internal/imgvips"
)

// s3Store adapts the S3 client to app.Store.
type s3Store struct{ c *s3.Client }

func (s s3Store) Head(ctx context.Context, bucket, key string) (map[string]string, int64, error) {
	o, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, 0, err
	}
	return o.Metadata, aws.ToInt64(o.ContentLength), nil
}

func (s s3Store) Get(ctx context.Context, bucket, key string) ([]byte, error) {
	o, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer o.Body.Close()
	return io.ReadAll(o.Body)
}

func (s s3Store) Put(ctx context.Context, bucket, key string, body []byte, contentType string, meta map[string]string) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &key, Body: bytes.NewReader(body), ContentType: &contentType, Metadata: meta,
	})
	return err
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	imgvips.Startup()
	defer imgvips.Shutdown()
	h := &app.Handler{
		Store:  s3Store{s3.NewFromConfig(cfg)},
		Resize: imgvips.Resize,
		Log:    slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
	lambda.Start(h.Handle)
}
