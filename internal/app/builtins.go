// Package app is the sole composition root for concrete gateway adapters.
package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"hoorific/internal/core"
	"hoorific/internal/provider/anthropic"
	"hoorific/internal/provider/antigravity"
	"hoorific/internal/provider/azureopenai"
	"hoorific/internal/provider/bedrock"
	"hoorific/internal/provider/claudesubscription"
	"hoorific/internal/provider/codexsubscription"
	"hoorific/internal/provider/cohere"
	"hoorific/internal/provider/compatible"
	"hoorific/internal/provider/endpoint"
	"hoorific/internal/provider/fal"
	"hoorific/internal/provider/gemini"
	"hoorific/internal/provider/geminicli"
	"hoorific/internal/provider/huggingface"
	"hoorific/internal/provider/kimisubscription"
	"hoorific/internal/provider/ollama"
	"hoorific/internal/provider/openai"
	"hoorific/internal/provider/replicate"
	"hoorific/internal/provider/vertex"
	"hoorific/internal/provider/xaisubscription"
)

type connectorFactory func(core.Connection) (core.Connector, error)
type connectionAdapter struct {
	base    core.Connector
	factory connectorFactory
	mu      sync.Mutex
	cache   map[string]cachedConnector
}
type cachedConnector struct {
	version   int64
	connector core.Connector
}

func (a *connectionAdapter) instance(c core.Connection) (core.Connector, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := c.TenantID + "/" + c.ID
	if v, ok := a.cache[key]; ok && v.version == c.Version {
		return v.connector, nil
	}
	v, e := a.factory(c)
	if e != nil {
		return nil, e
	}
	a.cache[key] = cachedConnector{version: c.Version, connector: v}
	return v, nil
}
func (a *connectionAdapter) Descriptor() core.ConnectorDescriptor { return a.base.Descriptor() }
func (a *connectionAdapter) DescriptorFor(c core.Connection) (core.ConnectorDescriptor, error) {
	instance, err := a.instance(c)
	if err != nil {
		return core.ConnectorDescriptor{}, err
	}
	if descriptor, ok := instance.(core.ConnectionDescriptor); ok {
		return descriptor.DescriptorFor(c)
	}
	return instance.Descriptor(), nil
}

func (a *connectionAdapter) APIKeyPolicyFor(c core.Connection) (core.APIKeyPolicy, error) {
	descriptor, err := a.DescriptorFor(c)
	if err != nil {
		return core.APIKeyPolicy{}, err
	}
	if descriptor.Subscription || c.Settings["auth_mode"] != "" && c.Settings["auth_mode"] != "api_key" || c.Settings["self_hosted"] == "true" && c.Settings["keyless_approved"] == "true" {
		return core.APIKeyPolicy{}, fmt.Errorf("configured connection does not accept API keys")
	}
	var policy core.APIKeyPolicy
	switch descriptor.ID {
	case "anthropic":
		policy = core.APIKeyPolicy{Header: "X-Api-Key"}
	case "gemini":
		policy = core.APIKeyPolicy{Header: "X-Goog-Api-Key"}
	case "azure-openai", "azure-openai-legacy":
		policy = core.APIKeyPolicy{Header: "Api-Key"}
	case "fal":
		policy = core.APIKeyPolicy{Header: "Authorization", Prefix: "Key"}
	case "openai", "cohere", "huggingface", "replicate", "ollama",
		"openrouter", "groq", "together", "fireworks", "deepseek", "deepseek-anthropic", "mistral", "xai", "cerebras":
		policy = core.APIKeyPolicy{Header: "Authorization", Prefix: "Bearer"}
	default:
		return core.APIKeyPolicy{}, fmt.Errorf("connector has no declared API-key policy")
	}
	// An unpreset compatible endpoint may declare its own identity header.
	// This is explicit operator configuration, never inferred from absent keys.
	if c.Connector == "compatible" && c.Settings["preset"] == "" && c.Settings["credential_header"] != "" {
		header := c.Settings["credential_header"]
		for _, ch := range header {
			if ch <= 32 || ch >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", ch) {
				return core.APIKeyPolicy{}, fmt.Errorf("invalid configured credential header")
			}
		}
		canonical := http.CanonicalHeaderKey(header)
		switch canonical {
		case "Host", "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "Cookie", "Forwarded":
			return core.APIKeyPolicy{}, fmt.Errorf("credential header is not an identity header")
		}
		if strings.HasPrefix(strings.ToLower(header), "x-forwarded-") {
			return core.APIKeyPolicy{}, fmt.Errorf("credential header is not an identity header")
		}
		prefix := c.Settings["credential_prefix"]
		if strings.TrimSpace(prefix) != prefix || strings.ContainsFunc(prefix, func(ch rune) bool { return ch < 32 || ch == 127 }) {
			return core.APIKeyPolicy{}, fmt.Errorf("invalid configured credential prefix")
		}
		return core.APIKeyPolicy{Header: canonical, Prefix: prefix}, nil
	}
	if header, ok := c.Settings["credential_header"]; ok && header != "" && !strings.EqualFold(header, policy.Header) {
		return core.APIKeyPolicy{}, fmt.Errorf("credential header does not match the connector policy")
	}
	if prefix, ok := c.Settings["credential_prefix"]; ok && !strings.EqualFold(prefix, policy.Prefix) {
		return core.APIKeyPolicy{}, fmt.Errorf("credential prefix does not match the connector policy")
	}
	return policy, nil
}
func (a *connectionAdapter) Endpoints() []core.NativeEndpoint {
	if v, ok := a.base.(core.EndpointInventory); ok {
		return v.Endpoints()
	}
	return nil
}
func (a *connectionAdapter) EndpointsFor(c core.Connection) ([]core.NativeEndpoint, error) {
	v, e := a.instance(c)
	if e != nil {
		return nil, e
	}
	if inv, ok := v.(core.ConnectionInventory); ok {
		return inv.EndpointsFor(c)
	}
	if inv, ok := v.(core.EndpointInventory); ok {
		return inv.Endpoints(), nil
	}
	return nil, fmt.Errorf("connector has no native endpoint inventory")
}
func targetConnection(t core.Target) core.Connection {
	switch v := t.(type) {
	case core.ModelCall:
		return v.Connection
	case core.ConnectionResourceCall:
		return v.Connection
	}
	return core.Connection{}
}
func (a *connectionAdapter) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	c, e := a.instance(targetConnection(t))
	if e != nil {
		return core.Binding{}, e
	}
	return c.Bind(ctx, t, o)
}
func (a *connectionAdapter) BindEndpoint(ctx context.Context, c core.Connection, e core.NativeEndpoint, p map[string]string) (core.Binding, error) {
	v, err := a.instance(c)
	if err != nil {
		return core.Binding{}, err
	}
	b, ok := v.(core.EndpointBinder)
	if !ok {
		return core.Binding{}, fmt.Errorf("connector cannot bind a native endpoint")
	}
	return b.BindEndpoint(ctx, c, e, p)
}
func (a *connectionAdapter) Inspect(ctx context.Context, t core.Target, o core.Operation, b []byte) error {
	v, e := a.instance(targetConnection(t))
	if e != nil {
		return e
	}
	if i, ok := v.(core.ScopeInspector); ok {
		return i.Inspect(ctx, t, o, b)
	}
	return nil
}
func (a *connectionAdapter) Discover(ctx context.Context, c core.Connection) ([]core.Model, error) {
	v, e := a.instance(c)
	if e != nil {
		return nil, e
	}
	if d, ok := v.(core.Discoverer); ok {
		return d.Discover(ctx, c)
	}
	return nil, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "connector has no catalog discovery"}
}

type connectionLease struct {
	source     core.CredentialSource
	connection core.Connection
}

func (l connectionLease) Authorize(ctx context.Context, r *http.Request) error {
	if l.source == nil {
		return fmt.Errorf("credential source unavailable")
	}
	v, e := l.source.Lease(ctx, l.connection)
	if e != nil {
		return e
	}
	if v == nil {
		return fmt.Errorf("credential lease unavailable")
	}
	authErr := v.Authorize(ctx, r)
	closeErr := closeCredentialLease(v)
	if authErr != nil {
		return authErr
	}
	return closeErr
}

func closeCredentialLease(v core.CredentialLease) error {
	switch closer := v.(type) {
	case interface{ Close() error }:
		return closer.Close()
	case interface{ Close() }:
		closer.Close()
	}
	return nil
}
func Builtins(client func(core.Connection) (*http.Client, error), credentials core.CredentialSource, subscriptions bool) map[string]core.Connector {
	result := make(map[string]core.Connector)
	add := func(id string, base core.Connector, f connectorFactory) {
		result[id] = &connectionAdapter{base: base, factory: f, cache: map[string]cachedConnector{}}
	}
	opts := func(c core.Connection) ([]endpoint.Option, error) {
		h, e := client(c)
		if e != nil {
			return nil, e
		}
		return []endpoint.Option{endpoint.WithDiscovery(h, credentials)}, nil
	}
	add("openai", openai.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return openai.New(o...), nil
	})
	add("anthropic", anthropic.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return anthropic.New(o...), nil
	})
	add("gemini", gemini.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return gemini.New(o...), nil
	})
	add("cohere", cohere.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return cohere.New(o...), nil
	})
	add("ollama", ollama.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return ollama.New(o...), nil
	})
	add("huggingface", huggingface.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return huggingface.New(o...), nil
	})
	add("replicate", replicate.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return replicate.New(o...), nil
	})
	add("fal", fal.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		return fal.New(o...), nil
	})
	add("azure-openai", azureopenai.New(), func(c core.Connection) (core.Connector, error) {
		h, e := client(c)
		if e != nil {
			return nil, e
		}
		o := []azureopenai.Option{azureopenai.WithHTTPClient(h), azureopenai.WithCredential(connectionLease{credentials, c})}
		if c.Settings["api_mode"] == "legacy" {
			return azureopenai.NewLegacy(o...), nil
		}
		return azureopenai.New(o...), nil
	})
	add("vertex", vertex.New(), func(c core.Connection) (core.Connector, error) {
		h, e := client(c)
		if e != nil {
			return nil, e
		}
		return vertex.New(vertex.WithHTTPClient(h), vertex.WithCredential(connectionLease{credentials, c})), nil
	})
	add("bedrock", bedrock.New(), func(c core.Connection) (core.Connector, error) {
		h, e := client(c)
		if e != nil {
			return nil, e
		}
		return bedrock.New(bedrock.WithHTTPClient(h), bedrock.WithCredential(connectionLease{credentials, c})), nil
	})
	// Compatible wire protocols are configuration, never provider branches in ingress.
	add("compatible", openai.New(), func(c core.Connection) (core.Connector, error) {
		o, e := opts(c)
		if e != nil {
			return nil, e
		}
		if preset := c.Settings["preset"]; preset != "" {
			return compatible.NewPreset(preset, o...)
		}
		switch c.Settings["protocol"] {
		case "anthropic-messages":
			return anthropic.New(o...), nil
		case "gemini-content":
			return gemini.New(o...), nil
		case "ollama":
			return ollama.New(o...), nil
		case "cohere-v2":
			return cohere.New(o...), nil
		case "openai-chat", "openai-responses", "openai-completion":
			return openai.New(o...), nil
		default:
			return nil, fmt.Errorf("compatible connection requires an explicit implemented protocol")
		}
	})
	if subscriptions {
		for _, c := range []core.Connector{codexsubscription.New(), claudesubscription.New(), geminicli.New(), antigravity.New(), kimisubscription.New(), xaisubscription.New()} {
			result[c.Descriptor().ID] = c
		}
	}
	return result
}
