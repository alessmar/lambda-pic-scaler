#!/usr/bin/env bash
# Deploys the resizer in three phases: 1) ECR stack, 2) image build + push, 3) Lambda stack.
# Usage: BUCKET_NAME=my-bucket ALARM_EMAIL=me@example.com [KEY_PREFIX=photos/] ./deploy.sh [ecr|image|app|all]
#        ./deploy.sh destroy    deletes both stacks (asks first; YES=1 skips the question)
# Needs the AWS CLI (with credentials and a region) and Docker.
# Optional: NAME_PREFIX (default lambda-pic-scaler), MAX_CONCURRENCY, INVOCATIONS_ALARM_THRESHOLD, NO_COLOR.
set -euo pipefail
cd "$(dirname "$0")"

phase=${1:-all}
NAME_PREFIX=${NAME_PREFIX:-lambda-pic-scaler}
ECR_STACK="$NAME_PREFIX-ecr"
APP_STACK="$NAME_PREFIX-app"

# Colors only on a terminal.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m' GREEN=$'\033[32m' RED=$'\033[31m' DIM=$'\033[2m' RESET=$'\033[0m'
else
  BOLD='' GREEN='' RED='' DIM='' RESET=''
fi
stage=''
registry='' # set once docker is logged in to it, so on_exit can log out
say() { printf '\n%s==>%s %s%s%s\n' "$GREEN" "$RESET" "$BOLD" "$*" "$RESET"; }
on_exit() {
  local rc=$?
  # Remove the ECR login that docker stored in ~/.docker/config.json, also when the run failed.
  [ -z "$registry" ] || docker logout "$registry" > /dev/null 2>&1 || true
  [ "$rc" -eq 0 ] || printf '\n%sFailed%s (exit %s)%s\n' "$RED" "${stage:+ during: $stage}" "$rc" "$RESET" >&2
}
trap on_exit EXIT

case $phase in
  ecr | image | destroy) ;;
  app | all)
    # Explicit tests: bash 3.2 (macOS) does not exit on ${VAR:?} and returns status 0.
    for var in BUCKET_NAME ALARM_EMAIL; do
      [ -n "${!var:-}" ] || { echo "set $var" >&2; exit 2; }
    done
    ;;
  *) echo "usage: $0 [ecr|image|app|all|destroy]" >&2; exit 2 ;;
esac

# The same commit gives the same tag in every phase; uncommitted changes get a unique one.
if [ -z "${IMAGE_TAG:-}" ]; then
  IMAGE_TAG=$(git rev-parse --short=12 HEAD)
  [ -z "$(git status --porcelain)" ] || IMAGE_TAG="$IMAGE_TAG-dirty-$(date +%s)"
fi

repo_uri() {
  aws cloudformation describe-stacks --stack-name "$ECR_STACK" \
    --query "Stacks[0].Outputs[?OutputKey=='RepositoryUri'].OutputValue" --output text
}

# Turns on EventBridge notifications and keeps every other notification of the bucket: the S3 API
# replaces the whole configuration on a write, so read it, add one key and write it back.
# shortcut: not atomic, a notification changed by someone else in that moment is lost, acceptable for a one-time setup.
ensure_eventbridge() {
  local on count merged
  on=$(aws s3api get-bucket-notification-configuration --bucket "$BUCKET_NAME" \
    --query "contains(keys(@), 'EventBridgeConfiguration')" --output json)
  [ "$on" = true ] && return 0
  count=$(aws s3api get-bucket-notification-configuration --bucket "$BUCKET_NAME" \
    --query 'length(keys(@))' --output json)
  case $count in
    0 | "" | null) merged='{"EventBridgeConfiguration": {}}' ;;
    *)
      # shellcheck disable=SC2016 # the backticks are JMESPath literals, not shell
      merged=$(aws s3api get-bucket-notification-configuration --bucket "$BUCKET_NAME" \
        --query 'merge(@, `{"EventBridgeConfiguration": {}}`)' --output json)
      ;;
  esac
  echo "Turning on EventBridge notifications for $BUCKET_NAME (its other notifications are kept)"
  aws s3api put-bucket-notification-configuration --bucket "$BUCKET_NAME" \
    --notification-configuration "$merged" || {
    echo "Could not update the notifications of $BUCKET_NAME. Add \"EventBridgeConfiguration\": {} to its" >&2
    echo "notification configuration yourself, then run this again." >&2
    exit 1
  }
}

phase_ecr() {
  aws cloudformation deploy --stack-name "$ECR_STACK" --template-file infra/ecr.yaml \
    --no-fail-on-empty-changeset --parameter-overrides NamePrefix="$NAME_PREFIX"
}

phase_image() {
  local uri
  uri=$(repo_uri)
  registry=${uri%%/*}
  aws ecr get-login-password | docker login --username AWS --password-stdin "$registry"
  # Lambda rejects the attestation manifests that docker adds by default; it runs x86_64 here.
  docker build --provenance=false --platform linux/amd64 -t "$uri:$IMAGE_TAG" .
  docker push "$uri:$IMAGE_TAG"
}

phase_app() {
  local extra=()
  [ -z "${MAX_CONCURRENCY:-}" ] || extra+=("MaxConcurrency=$MAX_CONCURRENCY")
  [ -z "${INVOCATIONS_ALARM_THRESHOLD:-}" ] || extra+=("InvocationsAlarmThreshold=$INVOCATIONS_ALARM_THRESHOLD")
  ensure_eventbridge
  aws cloudformation deploy --stack-name "$APP_STACK" --template-file template.yaml \
    --capabilities CAPABILITY_IAM CAPABILITY_NAMED_IAM CAPABILITY_AUTO_EXPAND \
    --no-fail-on-empty-changeset --parameter-overrides \
    NamePrefix="$NAME_PREFIX" ImageUri="$(repo_uri):$IMAGE_TAG" BucketName="$BUCKET_NAME" \
    KeyPrefix="${KEY_PREFIX:-}" AlarmEmail="$ALARM_EMAIL" ${extra[@]+"${extra[@]}"}
}

# Lambda stack first (it uses the image), then the ECR stack, which deletes its images too.
# The bucket, its images and its EventBridge notifications are not touched.
phase_destroy() {
  if [ "${YES:-}" != 1 ]; then
    [ -t 0 ] || { echo "Not a terminal: set YES=1 to confirm the deletion." >&2; exit 2; }
    printf 'Delete %s and %s (Lambda, queue with failed events, alarms, ECR repository and its images)? [y/N] ' "$APP_STACK" "$ECR_STACK"
    read -r answer
    [ "$answer" = y ] || { echo "Cancelled, nothing was deleted."; exit 0; }
  fi
  aws cloudformation delete-stack --stack-name "$APP_STACK"
  aws cloudformation wait stack-delete-complete --stack-name "$APP_STACK"
  aws cloudformation delete-stack --stack-name "$ECR_STACK"
  aws cloudformation wait stack-delete-complete --stack-name "$ECR_STACK"
  echo "Deleted. The bucket and its images are untouched, and its EventBridge notifications stay on."
}

run_phase() { # title function
  stage=$1
  say "$1"
  SECONDS=0
  "$2"
  printf '%s    done in %ss%s\n' "$DIM" "$SECONDS" "$RESET"
}

summary() {
  say "Deployed"
  printf '  %-9s %s-resizer\n' Function "$NAME_PREFIX"
  printf '  %-9s %s:%s\n' Image "$(repo_uri)" "$IMAGE_TAG"
  printf '  %-9s s3://%s/%s\n' Watching "$BUCKET_NAME" "${KEY_PREFIX:-}"
  printf '  %-9s aws logs tail /aws/lambda/%s-resizer --follow\n' Logs "$NAME_PREFIX"
  printf '\nConfirm the subscription email AWS sent to %s, or the alarms reach nobody.\n' "$ALARM_EMAIL"
}

# Fail early, before the long image build, if the credentials are missing.
account=$(aws sts get-caller-identity --query Account --output text)
region=${AWS_REGION:-${AWS_DEFAULT_REGION:-$(aws configure get region)}}
say "Account $account, region $region, name prefix $NAME_PREFIX, image tag $IMAGE_TAG"

case $phase in
  destroy) run_phase "Delete $APP_STACK, then $ECR_STACK" phase_destroy ;;
  ecr) run_phase "Phase 1/3: ECR stack ($ECR_STACK)" phase_ecr ;;
  image) run_phase "Phase 2/3: build and push the image" phase_image ;;
  app) run_phase "Phase 3/3: Lambda stack ($APP_STACK)" phase_app; summary ;;
  all)
    run_phase "Phase 1/3: ECR stack ($ECR_STACK)" phase_ecr
    run_phase "Phase 2/3: build and push the image" phase_image
    run_phase "Phase 3/3: Lambda stack ($APP_STACK)" phase_app
    summary
    ;;
esac
