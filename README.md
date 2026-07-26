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

## Deploy

The deployment has three phases, and each one finishes before the next starts:

1. **ECR stack** (`<NamePrefix>-ecr`): the repository that holds the Lambda image.
2. **Image**: `docker build` and `docker push`, tagged with the Git commit.
3. **Lambda stack** (`<NamePrefix>-app`): the Lambda, its role, the EventBridge rule, the dead-letter queue and the alarms, created from the image of phase 2.

You can run them from your own machine or from GitHub Actions. The bucket must already exist. The pipeline turns on the bucket's EventBridge notifications itself, in phase 3. If the bucket already has other notifications (a Lambda, a queue or a topic), it reads them, adds the one `"EventBridgeConfiguration": {}` setting and writes everything back, so they keep working. S3 replaces the whole configuration on every write, so a change made by someone else in that same second could be lost. If the write fails (for example for lack of permission), the pipeline stops and asks you to add that setting to the bucket's notification configuration yourself and run it again.

### Option A: from your machine

You need Git, Docker (running) and the AWS CLI v2 with credentials.

1. Check out the repository:

   ```sh
   git clone https://github.com/alessmar/lambda-pic-scaler.git
   cd lambda-pic-scaler
   ```

2. Point the AWS CLI at the account and region that hold your bucket, and check it:

   ```sh
   export AWS_PROFILE=my-profile
   export AWS_REGION=eu-west-1
   aws sts get-caller-identity
   ```

3. Run the three phases. The first run takes several minutes, mostly the image build:

   ```sh
   BUCKET_NAME=my-bucket ALARM_EMAIL=me@example.com KEY_PREFIX=photos/ ./deploy.sh
   ```

   `KEY_PREFIX` is optional (leave it out to watch the whole bucket). The output is colored on a terminal (set `NO_COLOR=1` to turn that off), shows each phase with its duration, and ends with a summary. You can run a single phase with `./deploy.sh ecr`, `./deploy.sh image` or `./deploy.sh app`. More optional variables: `NAME_PREFIX`, `MAX_CONCURRENCY` and `INVOCATIONS_ALARM_THRESHOLD` (see the parameters below).

4. Open the email AWS sends to `ALARM_EMAIL` and confirm the subscription. Until you do, alarms reach nobody.

5. Try it:

   ```sh
   aws s3 cp a.jpg s3://my-bucket/photos/a.jpg \
     --metadata '{"resize":"[{\"w\":200,\"postfix\":\"-thumb\"}]"}'
   aws s3 ls s3://my-bucket/photos/
   aws logs tail /aws/lambda/lambda-pic-scaler-resizer --follow
   ```

   `photos/a-thumb.jpg` should appear within a few seconds.

6. To update after a code change, commit and run `./deploy.sh` again: a new commit makes a new image tag.

7. To remove everything, run `./deploy.sh destroy`. It asks first, then deletes the Lambda stack and the ECR stack (the Lambda, its role and log group, the queue with any failed events, the alarms, the repository and its images). In a script, skip the question with `YES=1 ./deploy.sh destroy`. The bucket and its images, including the resized copies, are not touched, and its EventBridge notifications stay on.

### Option B: from GitHub Actions

Nothing runs on your machine except a one-time setup of the AWS role. No AWS keys are stored in GitHub: the workflow gets short-lived credentials through OIDC.

1. Fork `alessmar/lambda-pic-scaler` on GitHub, clone your fork, and in the fork's Actions tab enable workflows (GitHub disables them in new forks).

2. Create the AWS role that lets your fork deploy, once. Use your fork's `owner/name`, and add `CreateOidcProvider=false` if the account already has the GitHub OIDC provider:

   ```sh
   aws cloudformation deploy \
     --template-file infra/github-oidc.yaml \
     --stack-name lambda-pic-scaler-github-oidc \
     --capabilities CAPABILITY_NAMED_IAM \
     --parameter-overrides GitHubRepo=me/lambda-pic-scaler BucketName=my-bucket
   ```

   Only workflows of that repository, running on the `main` branch, can use the role, and its permissions cover only this project's resources.

3. Give the workflow the role and the region as repository variables (with the GitHub CLI, or in Settings, Secrets and variables, Actions, Variables):

   ```sh
   ROLE=$(aws cloudformation describe-stacks --stack-name lambda-pic-scaler-github-oidc \
     --query "Stacks[0].Outputs[?OutputKey=='RoleArn'].OutputValue" --output text)
   gh variable set AWS_ROLE_ARN --body "$ROLE"
   gh variable set AWS_REGION --body eu-west-1
   ```

4. Run the workflow, from the Actions tab (choose "deploy", then "Run workflow" and fill the form) or from a terminal:

   ```sh
   gh workflow run deploy.yml -f bucket_name=my-bucket -f alarm_email=me@example.com -f key_prefix=photos/
   ```

   The run shows the three phases as three steps.

5. Confirm the alarm email and try it, as in steps 4 and 5 of option A. To update, run the workflow again. To remove everything, run `./deploy.sh destroy` from your machine (step 7 of option A), then delete the one-time role with `aws cloudformation delete-stack --stack-name lambda-pic-scaler-github-oidc`.

### Parameters of the Lambda stack

| Parameter | Default | Purpose |
|---|---|---|
| `BucketName` | none | The existing bucket. |
| `AlarmEmail` | none | Receives the alarms. |
| `ImageUri` | none | Image address, filled in by the pipeline. |
| `KeyPrefix` | empty | Only objects under it trigger the function. |
| `NamePrefix` | `lambda-pic-scaler` | Every named resource is `<NamePrefix>-<purpose>`, for example `lambda-pic-scaler-resizer` or `lambda-pic-scaler-failed-events`. |
| `MaxConcurrency` | 10 | Hard cap on parallel runs. It needs that much unreserved concurrency left in the account (minimum 10), so on a new account raise the Lambda concurrency quota in Service Quotas or lower this value. |
| `InvocationsAlarmThreshold` | 1000 | The loop alarm fires at this many invocations in 5 minutes. Raise it for bulk imports. |

## Develop

The loop guards, the parsing and the deploy script have no native dependencies, and run on every push in GitHub Actions:

```sh
go test ./scaler ./internal/app
./test/deploy_test.sh
```

The `libvips` package needs `libvips-dev` and a C compiler. Without installing them, run everything in a container:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.27-trixie sh -c \
  'apt-get update -qq && apt-get install -y -qq libvips-dev && go test ./...'
```
