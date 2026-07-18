# lambda-pic-scaler

A Go-based AWS Lambda function for resizing images on the fly, designed for serverless image processing in cloud applications

## Why this project

At work I built almost the same Lambda in Node.js, using `sharp`, which is built on `libvips`, for our custom-made e-commerce. The sizes to create were defined in a config file keyed by bucket name and key prefix. It served us well for years, until we moved to a hosted platform that does the same job, which made the Lambda unnecessary.

I still thought it would be nice to rewrite it in Go and make it declarative: instead of a config file, the instructions travel with each image as S3 object metadata. Whoever uploads a file decides which sizes it needs, and nothing has to be redeployed when that changes.

## How it works

1. An image is uploaded to an existing bucket with the user metadata `resize` (header `x-amz-meta-resize`).
2. S3 sends an "Object Created" event to EventBridge, which invokes the Lambda.
3. The Lambda reads the metadata with `HeadObject`, resizes the image with `libvips` and writes one copy per entry next to the original.

The metadata is a JSON array (max 10 entries):

```json
[{"w": 1200, "h": 800, "postfix": "-large"}, {"w": 200, "postfix": "-thumb"}]
```

- `w` / `h`: max 8192, and neither side of the output ever exceeds 8192. With both set the image is fitted inside the box (aspect ratio kept); with one set the other is derived, so a very tall or wide image can end up smaller than the requested side. Images smaller than the requested size are enlarged to it, so the output always has the size you ask for (enlarging adds no detail).
- `postfix`: required, inserted before the extension (`photos/a.jpg` becomes `photos/a-large.jpg`).
- Supported formats: JPEG, PNG, WebP. The output keeps the input format, is oriented by EXIF and has its metadata stripped.
- Inputs above 50 MB, invalid metadata and unsupported files are logged and skipped, not retried.

S3 limits all user metadata of an object to 2 KB, which is plenty for a handful of sizes.

With the AWS CLI, pass `--metadata` as a JSON map whose `resize` value is the instructions as an escaped string. The shorthand form `resize=[...]` does not work, because it splits on commas:

```sh
aws s3 cp a.jpg s3://my-bucket/photos/a.jpg \
  --metadata '{"resize":"[{\"w\":200,\"postfix\":\"-thumb\"}]"}'
```

## Loop protection

The outputs land in the bucket that triggers the Lambda, so a mistake here would make it feed itself. Several independent guards prevent that:

- Every output is stamped with the metadata `resized-by` and the Lambda ignores any object carrying it, without downloading it.
- Outputs never get the `resize` instructions, and an object without instructions is ignored. This holds even if all metadata were lost.
- A spec whose output key equals the input key is refused, and an empty postfix is invalid.
- At most 10 outputs per upload; each retry of a failed event rewrites the same keys, and a failed event is retried once, then goes to a dead-letter queue.
- The function has a reserved concurrency cap (`MaxConcurrency`) that bounds the spend rate whatever happens, and an alarm fires when invocations exceed `InvocationsAlarmThreshold` per 5 minutes.

`internal/app/app_test.go` simulates a bucket that emits an event for every write, like S3 does, and fails if one upload costs more than 1 + N invocations. Run it with `go test ./internal/app`.
