# libvips needs cgo and shared libraries, so this Lambda ships as a container image, built and pushed
# by deploy.sh (phase 2). Any Linux image works: aws-lambda-go talks to the Lambda Runtime API directly.
# Base images come from the ECR Public mirror of Docker Hub, which avoids Docker Hub pull limits.
FROM public.ecr.aws/docker/library/golang:1.27-trixie AS build
RUN apt-get update && apt-get install -y --no-install-recommends libvips-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/bootstrap ./cmd/resizer

FROM public.ecr.aws/docker/library/debian:trixie-slim
# Same Debian release as the build stage, so the libvips ABI matches.
RUN apt-get update && apt-get install -y --no-install-recommends libvips42t64 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/bootstrap /bootstrap
ENTRYPOINT ["/bootstrap"]
