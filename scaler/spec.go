// Package scaler parses resize instructions and applies them to images.
package scaler

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/go-playground/validator/v10"
)

// MetadataKey is the S3 user-metadata key (sent as x-amz-meta-resize) holding the instructions.
// The S3 API returns user-metadata keys lowercased.
const MetadataKey = "resize"

// OutputMarkerKey is the user-metadata key stamped on every object this service writes.
// Objects carrying it are never processed, which is what breaks the S3 -> Lambda -> S3 loop.
// The outputs also never carry MetadataKey, so a second, independent guard applies.
const (
	OutputMarkerKey   = "resized-by"
	OutputMarkerValue = "lambda-pic-scaler"
)

// MaxSpecs caps the fan-out per input object.
const MaxSpecs = 10

// ErrUnsupported means the data is not an image format we handle. It is permanent, never retry.
var ErrUnsupported = errors.New("unsupported image format")

// MaxDim caps width and height to bound memory use. Keep it equal to the max= in Spec's tags
// (a tag cannot use a constant); TestMaxDimMatchesTags fails if they drift apart.
const MaxDim = 8192

// Spec is one output size. W or H may be 0 to keep the aspect ratio;
// when both are set the image is fitted inside the W x H box (no distortion).
// The image is enlarged when it is smaller than that.
type Spec struct {
	W int `json:"w" validate:"min=0,max=8192"`
	// H is required when W is 0, so the two can never both be 0.
	H int `json:"h" validate:"min=0,max=8192,required_without=W"`
	// An empty postfix would overwrite the original object, and a path separator would leave its folder.
	Postfix string `json:"postfix" validate:"required,excludesall=/\\"`
}

// specsRule applies to the whole list: size, then every postfix unique (they name the output
// objects), then each element against its own tags.
var specsRule = fmt.Sprintf("min=1,max=%d,unique=Postfix,dive", MaxSpecs)

var validate = validator.New(validator.WithRequiredStructEnabled())

// Parse decodes and validates a JSON array of specs, e.g. [{"w":800,"h":600,"postfix":"-md"}].
func Parse(raw string) ([]Spec, error) {
	var specs []Spec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, fmt.Errorf("invalid %s metadata: %w", MetadataKey, err)
	}
	if err := validate.Var(specs, specsRule); err != nil {
		return nil, fmt.Errorf("invalid %s metadata: %w", MetadataKey, describe(err))
	}
	return specs, nil
}

// describe shortens the library's messages to "[0].W max=8192".
func describe(err error) error {
	ves, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return err
	}
	msgs := make([]string, len(ves))
	for i, e := range ves {
		msgs[i] = strings.TrimSpace(e.Namespace() + " " + e.Tag() + "=" + e.Param())
	}
	return errors.New(strings.Join(msgs, "; "))
}

// OutputKey inserts the postfix before the extension: a/b.jpg + "-md" -> a/b-md.jpg.
func (s Spec) OutputKey(key string) string {
	ext := path.Ext(key)
	return strings.TrimSuffix(key, ext) + s.Postfix + ext
}
