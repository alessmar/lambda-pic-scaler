#!/usr/bin/env bash
# Runs deploy.sh against stub aws/docker/git commands and checks the order of calls.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

cat > "$tmp/bin/aws" <<'STUB'
#!/usr/bin/env bash
echo "aws $*" >> "$STUB_LOG"
case "$*" in
  *"describe-stacks"*) echo "111.dkr.ecr.eu-west-1.amazonaws.com/lambda-pic-scaler-resizer-image" ;;
  *"contains(keys"*) echo "$STUB_ON" ;;
  *"length(keys"*) echo "$STUB_COUNT" ;;
  *"merge("*) echo "$STUB_MERGED" ;;
  *"put-bucket-notification-configuration"*) if [ "${STUB_FAIL_PUT:-}" = 1 ]; then exit 1; fi ;;
  *"get-login-password"*) echo password ;;
  *"get-caller-identity"*) echo 111111111111 ;;
  *"configure get region"*) echo eu-west-1 ;;
esac
STUB
cat > "$tmp/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >> "$STUB_LOG"
[ "${STUB_FAIL_PUSH:-}" = 1 ] && [ "$1" = push ] && exit 1
cat > /dev/null < /dev/stdin 2>/dev/null || true
STUB
cat > "$tmp/bin/git" <<'STUB'
#!/usr/bin/env bash
case "$*" in "rev-parse"*) echo abc123def456 ;; esac
STUB
chmod +x "$tmp/bin/"*

run() { # on count args... ; env BUCKET_NAME / ALARM_EMAIL come from the caller
  : > "$tmp/log"
  PATH="$tmp/bin:$PATH" STUB_LOG="$tmp/log" STUB_ON=$1 STUB_COUNT=$2 "$root/deploy.sh" "${@:3}" > "$tmp/out" 2>&1
}
fail() { echo "FAIL: $1"; cat "$tmp/log"; exit 1; }
order() { grep -n -F -- "$1" "$tmp/log" | head -1 | cut -d: -f1; }
export BUCKET_NAME=b ALARM_EMAIL=a@example.com

# 1. clean bucket: three phases in order, EventBridge enabled before the app stack, tag from git
run false 0 all || fail "all, empty bucket"
[ "$(order 'deploy --stack-name lambda-pic-scaler-ecr')" -lt "$(order 'docker build')" ] || fail "ecr before build"
[ "$(order 'docker push')" -lt "$(order 'put-bucket-notification-configuration')" ] || fail "push before app phase"
[ "$(order 'put-bucket-notification-configuration')" -lt "$(order 'deploy --stack-name lambda-pic-scaler-app')" ] || fail "eventbridge before app"
grep -q 'ImageUri=111.dkr.ecr.eu-west-1.amazonaws.com/lambda-pic-scaler-resizer-image:abc123def456' "$tmp/log" || fail "image uri"
grep -q -- '--provenance=false' "$tmp/log" || fail "provenance flag"

# 2. EventBridge already on: never touches the bucket configuration
run true 1 all || fail "already enabled"
! grep -q 'put-bucket-notification-configuration' "$tmp/log" || fail "wrote config although enabled"

# 3. bucket with other notifications: they are kept (merged config is written), then the app stack deploys
merged='{"QueueConfigurations":[{"Id":"x"}],"EventBridgeConfiguration":{}}'
STUB_MERGED=$merged run false 2 all || fail "bucket with notifications"
grep -q -F -- "--notification-configuration $merged" "$tmp/log" || fail "existing notifications were not kept"
! grep -q -F -- '--notification-configuration {"EventBridgeConfiguration": {}}' "$tmp/log" || fail "wrote the bare config over existing notifications"
[ "$(order 'put-bucket-notification-configuration')" -lt "$(order 'deploy --stack-name lambda-pic-scaler-app')" ] || fail "merge before app"

# 3b. the write fails: stop before the app stack and say what to do by hand
if STUB_MERGED=$merged STUB_FAIL_PUT=1 run false 2 all; then fail "failed write reported success"; fi
! grep -q 'deploy --stack-name lambda-pic-scaler-app' "$tmp/log" || fail "deployed app after a failed write"
grep -q 'yourself' "$tmp/out" || fail "no manual fallback message"

# 4. missing parameters fail before any call; single phases run alone
if (unset BUCKET_NAME; run false 0 all); then fail "missing bucket accepted"; fi
[ ! -s "$tmp/log" ] || fail "calls made without BUCKET_NAME"
run false 0 ecr || fail "ecr alone"
[ "$(grep -c 'cloudformation deploy' "$tmp/log")" = 1 ] || fail "ecr phase ran more than one deploy"
! grep -q 'docker' "$tmp/log" || fail "ecr phase touched docker"
# 5. optional stack parameters are passed only when set
run true 1 app || fail "app alone"
! grep -q 'MaxConcurrency' "$tmp/log" || fail "MaxConcurrency passed although unset"
MAX_CONCURRENCY=5 run true 1 app || fail "app with MAX_CONCURRENCY"
grep -q 'MaxConcurrency=5' "$tmp/log" || fail "MaxConcurrency not passed"

# 6. output: phase banners and a summary, and no color codes when not on a terminal
run false 0 all || fail "all for output"
for text in 'Phase 1/3' 'Phase 3/3' 'Deployed'; do
  grep -q "$text" "$tmp/out" || fail "output lacks: $text"
done
! grep -q $'\033' "$tmp/out" || fail "color codes on a non-terminal"
if (unset ALARM_EMAIL; run false 0 all); then fail "missing email accepted"; fi
grep -q 'ALARM_EMAIL' "$tmp/out" || fail "no hint for the missing variable"

# 7. the docker login is removed when the run ends, also when the push fails
run false 0 all || fail "all for logout"
[ "$(order 'docker push')" -lt "$(order 'docker logout')" ] || fail "logout after push"
grep -q 'docker logout 111.dkr.ecr.eu-west-1.amazonaws.com' "$tmp/log" || fail "logout of the registry"
if STUB_FAIL_PUSH=1 run false 0 image; then fail "failed push reported success"; fi
grep -q 'docker logout' "$tmp/log" || fail "no logout after a failed push"
run false 0 ecr || fail "ecr for logout"
! grep -q 'docker logout' "$tmp/log" || fail "logout without a login"

# 8. destroy: app stack before the ECR stack, waits in between, and only with confirmation
if run false 0 destroy; then fail "destroy ran without confirmation on a non-terminal"; fi
! grep -q 'delete-stack' "$tmp/log" || fail "deleted without confirmation"
(unset BUCKET_NAME ALARM_EMAIL; YES=1 run false 0 destroy) || fail "destroy with YES=1"
[ "$(order 'delete-stack --stack-name lambda-pic-scaler-app')" -lt "$(order 'wait stack-delete-complete --stack-name lambda-pic-scaler-app')" ] || fail "wait after app delete"
[ "$(order 'wait stack-delete-complete --stack-name lambda-pic-scaler-app')" -lt "$(order 'delete-stack --stack-name lambda-pic-scaler-ecr')" ] || fail "app gone before ecr delete"
[ "$(order 'delete-stack --stack-name lambda-pic-scaler-ecr')" -lt "$(order 'wait stack-delete-complete --stack-name lambda-pic-scaler-ecr')" ] || fail "wait after ecr delete"
! grep -q -E 'docker|s3api' "$tmp/log" || fail "destroy touched docker or the bucket"

echo "deploy.sh: all checks passed"
