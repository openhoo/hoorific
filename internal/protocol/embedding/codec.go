// Package embedding translates text embedding payloads without discarding semantics.
package embedding

import (
	"hoorific/internal/core"
)

// Codec implements the shared request and result codec contracts.
// Its zero value, like New, selects OpenAI.
type Codec struct{ variant string }

func New() *Codec                      { return NewVariant("openai") }
func NewVariant(variant string) *Codec { return &Codec{variant: variant} }

func (c *Codec) selected() (string, error) {
	if c == nil {
		return "", unsupported("variant", "nil embedding codec")
	}
	switch c.variant {
	case "", "openai":
		return "openai", nil
	case "gemini", "gemini-batch", "cohere-v1", "cohere-v2", "ollama", "ollama-legacy":
		return c.variant, nil
	default:
		return "", unsupported("variant", "unsupported embedding variant")
	}
}

func unsupported(field, message string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: message, Origin: "gateway"}
}

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
