// Package rerank implements the string-document subset of Cohere rerank JSON.
// The exact variants are cohere-v1 (/v1/rerank) and cohere-v2 (/v2/rerank).
// Their representable request and result fields have the same JSON shape.
// Neither variant accepts document objects, returned documents, response IDs,
// API-version metadata, search-unit billing, or provider-specific options:
// those values have no lossless representation in core's rerank contracts.
package rerank

import (
	"context"
	"io"

	"hoorific/internal/core"
)

const (
	VariantV1 = "cohere-v1"
	VariantV2 = "cohere-v2"
)

// Codec converts rerank requests and results; its zero value uses cohere-v2.
// Constructed codecs are immutable and can be shared between goroutines.
type Codec struct{ variant string }

var (
	_ core.RequestCodec = (*Codec)(nil)
	_ core.ResultCodec  = (*Codec)(nil)
)

// New returns the default cohere-v2 codec.
func New() *Codec { return NewVariant(VariantV2) }

// NewVariant selects an exact wire variant. Unsupported names, including an
// explicitly empty name, are rejected by all codec methods, not normalized.
func NewVariant(variant string) *Codec {
	if variant == "" {
		return &Codec{variant: "unsupported-empty-variant"}
	}
	return &Codec{variant: variant}
}

func (c *Codec) checkVariant() error {
	if c == nil {
		return unsupported("variant")
	}
	switch c.variant {
	case "", VariantV1, VariantV2:
		return nil
	default:
		return unsupported("variant")
	}
}

func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if err := c.checkVariant(); err != nil {
		return nil, err
	}
	return decodeRequest(ctx, r)
}

func (c *Codec) EncodeRequest(ctx context.Context, payload core.RequestPayload, w io.Writer) error {
	if err := c.checkVariant(); err != nil {
		return err
	}
	return encodeRequest(ctx, payload, w)
}

func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	if err := c.checkVariant(); err != nil {
		return nil, err
	}
	return decodeResult(ctx, r)
}

func (c *Codec) EncodeResult(ctx context.Context, payload core.ResultPayload, w io.Writer) error {
	if err := c.checkVariant(); err != nil {
		return err
	}
	return encodeResult(ctx, payload, w)
}

func unsupported(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "Rerank field or value cannot be preserved", Origin: "gateway"}
}
