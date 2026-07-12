// Package app holds the handler logic, free of AWS SDK and libvips so the loop guards are unit-testable.
package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambdacontext"

	"github.com/alessmar/lambda-pic-scaler/scaler"
)

// MaxInputBytes skips oversized inputs, to bound memory and run time (and so cost).
const MaxInputBytes = 50 << 20

// Store is the slice of S3 the handler needs. Metadata keys may come back in any case.
type Store interface {
	Head(ctx context.Context, bucket, key string) (meta map[string]string, size int64, err error)
	Get(ctx context.Context, bucket, key string) ([]byte, error)
	Put(ctx context.Context, bucket, key string, body []byte, contentType string, meta map[string]string) error
}

// ResizeFunc returns the resized bytes and their content type, or scaler.ErrUnsupported.
type ResizeFunc func(src []byte, spec scaler.Spec) (out []byte, contentType string, err error)

type Handler struct {
	Store  Store
	Resize ResizeFunc
	Log    *slog.Logger
}

// detail is the part of an EventBridge "Object Created" event that we need.
type detail struct {
	Bucket struct {
		Name string `json:"name"`
	} `json:"bucket"`
	Object struct {
		Key string `json:"key"` // not URL-encoded, unlike classic S3 notifications
	} `json:"object"`
}

func (h *Handler) Handle(ctx context.Context, ev events.CloudWatchEvent) error {
	log := h.Log
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		log = log.With("request_id", lc.AwsRequestID)
	}
	var d detail
	if err := json.Unmarshal(ev.Detail, &d); err != nil {
		log.Warn("skipped: unreadable event, not retrying", "error", err)
		return nil
	}
	log = log.With("bucket", d.Bucket.Name, "key", d.Object.Key)
	// Returning an error makes Lambda retry the event (bounded by the template), then DLQ it.
	// Re-processing is idempotent: outputs are written to deterministic keys.
	if err := h.process(ctx, log, d.Bucket.Name, d.Object.Key); err != nil {
		log.Error("failed", "error", err)
		return err
	}
	return nil
}

func lookup(meta map[string]string, key string) (string, bool) {
	for k, v := range meta {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func (h *Handler) process(ctx context.Context, log *slog.Logger, bucket, key string) error {
	meta, size, err := h.Store.Head(ctx, bucket, key)
	if err != nil {
		return err
	}
	// LOOP GUARD 1: never process our own output, even if it somehow carries instructions.
	if _, own := lookup(meta, scaler.OutputMarkerKey); own {
		log.Info("skipped: output of this service")
		return nil
	}
	// LOOP GUARD 2: outputs never get instructions, so no instructions means nothing to do.
	raw, ok := lookup(meta, scaler.MetadataKey)
	if !ok {
		log.Info("skipped: no resize metadata")
		return nil
	}
	if size > MaxInputBytes {
		log.Warn("skipped: input too large", "bytes", size)
		return nil
	}
	specs, err := scaler.Parse(raw)
	if err != nil {
		log.Warn("skipped: bad metadata, not retrying", "error", err)
		return nil
	}
	src, err := h.Store.Get(ctx, bucket, key)
	if err != nil {
		return err
	}
	// Resize everything before writing anything: no partial output on a bad spec or image.
	type out struct {
		key, ct string
		body    []byte
	}
	var outs []out
	for _, spec := range specs {
		outKey := spec.OutputKey(key)
		if outKey == key { // LOOP GUARD 3: never overwrite the input
			log.Warn("skipped: output key equals input key", "postfix", spec.Postfix)
			return nil
		}
		body, ct, err := h.Resize(src, spec)
		if errors.Is(err, scaler.ErrUnsupported) {
			log.Warn("skipped: unsupported image, not retrying")
			return nil
		}
		if err != nil {
			return err
		}
		outs = append(outs, out{outKey, ct, body})
	}
	for _, o := range outs {
		// Output metadata: the marker only, never the instructions.
		err := h.Store.Put(ctx, bucket, o.key, o.body, o.ct,
			map[string]string{scaler.OutputMarkerKey: scaler.OutputMarkerValue})
		if err != nil {
			return err
		}
		log.Info("resized", "out_key", o.key, "bytes", len(o.body))
	}
	return nil
}
