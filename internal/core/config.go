package core

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strings"
	"unicode"
)

type BootstrapConfig struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          string `json:"mode"`
	DataDir       string `json:"data_dir"`
	Listeners     struct {
		Inference  string `json:"inference"`
		Management string `json:"management"`
	} `json:"listeners"`
	Storage struct {
		SQLite struct {
			Path string `json:"path"`
		} `json:"sqlite"`
		Postgres struct {
			DSNFile string `json:"dsn_file"`
		} `json:"postgres"`
	} `json:"storage"`
	Coordination struct {
		Redis struct {
			URLFile string `json:"url_file"`
		} `json:"redis"`
	} `json:"coordination"`
	Encryption struct {
		KeyFile string `json:"key_file"`
	} `json:"encryption"`
	OIDC struct {
		Issuer           string `json:"issuer"`
		ClientID         string `json:"client_id"`
		ClientSecretFile string `json:"client_secret_file"`
	} `json:"oidc"`
	PublicURLs             map[string]string `json:"public_urls"`
	SubscriptionConnectors struct {
		Enabled bool `json:"enabled"`
	} `json:"subscription_connectors"`
	OAuth     map[string]OAuthRegistration `json:"oauth,omitempty"`
	Transport TransportConfig              `json:"transport,omitempty"`
	Telemetry TelemetryConfig              `json:"telemetry,omitempty"`
}

// TelemetryConfig describes the optional OpenTelemetry SDK wiring. An empty
// exporter list means that the standard OTEL_* environment configuration is
// used when telemetry is enabled.
type TelemetryConfig struct {
	Enabled        bool                `json:"enabled"`
	ServiceName    string              `json:"service_name,omitempty"`
	ServiceVersion string              `json:"service_version,omitempty"`
	Environment    string              `json:"environment,omitempty"`
	SampleRatio    *float64            `json:"sample_ratio,omitempty"`
	Exporters      []TelemetryExporter `json:"exporters,omitempty"`
}

// TelemetryExporter is one independent destination. Signals may be empty,
// which selects all three signals.
type TelemetryExporter struct {
	Name        string   `json:"name"`
	Signals     []string `json:"signals,omitempty"`
	Protocol    string   `json:"protocol"`
	Endpoint    string   `json:"endpoint"`
	HeadersFile string   `json:"headers_file,omitempty"`
	Insecure    bool     `json:"insecure,omitempty"`
}

const (
	DefaultTelemetrySampleRatio = 1.0
	MaximumTelemetryExporters   = 16
)

// EffectiveSampleRatio returns the configured ratio, or the safe default when
// the field was omitted. A pointer preserves an explicit zero (and explicit
// one) so callers can distinguish it from an omitted field.
func (c TelemetryConfig) EffectiveSampleRatio() float64 {
	if c.SampleRatio == nil {
		return DefaultTelemetrySampleRatio
	}
	return *c.SampleRatio
}

var telemetrySignals = map[string]struct{}{
	"traces":  {},
	"metrics": {},
	"logs":    {},
}

func validTelemetryText(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if len(value) > 2048 {
		return fmt.Errorf("telemetry.%s is too long", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("telemetry.%s contains a control character", name)
		}
	}
	return nil
}

func DecodeBootstrap(r io.Reader) (BootstrapConfig, error) {
	var c BootstrapConfig
	c.Mode = "standalone"
	c.Listeners.Inference = ":8080"
	c.Listeners.Management = "127.0.0.1:8081"
	// A nil SampleRatio intentionally leaves the standard OTEL sampler
	// environment eligible; EffectiveSampleRatio supplies the runtime default.
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	return c, c.Validate()
}

func (c TelemetryConfig) Validate() error {
	if c.SampleRatio != nil {
		ratio := *c.SampleRatio
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
			return fmt.Errorf("telemetry.sample_ratio must be between 0 and 1")
		}
	}
	for _, field := range []struct {
		name, value string
	}{
		{"service_name", c.ServiceName},
		{"service_version", c.ServiceVersion},
		{"environment", c.Environment},
	} {
		if err := validTelemetryText(field.name, field.value); err != nil {
			return err
		}
	}
	if len(c.Exporters) > MaximumTelemetryExporters {
		return fmt.Errorf("telemetry.exporters must contain at most %d entries", MaximumTelemetryExporters)
	}
	names := make(map[string]struct{}, len(c.Exporters))
	for i, exporter := range c.Exporters {
		prefix := fmt.Sprintf("telemetry.exporters[%d]", i)
		if strings.TrimSpace(exporter.Name) == "" {
			return fmt.Errorf("%s.name must be configured", prefix)
		}
		if _, exists := names[exporter.Name]; exists {
			return fmt.Errorf("%s.name must be unique", prefix)
		}
		names[exporter.Name] = struct{}{}
		if err := validTelemetryText(prefix+".name", exporter.Name); err != nil {
			return err
		}
		switch exporter.Protocol {
		case "http/protobuf", "grpc":
		default:
			return fmt.Errorf("%s.protocol must be http/protobuf or grpc", prefix)
		}
		if strings.TrimSpace(exporter.Endpoint) == "" {
			return fmt.Errorf("%s.endpoint must be configured", prefix)
		}
		if len(exporter.Endpoint) > 4096 || strings.ContainsAny(exporter.Endpoint, "\r\n") {
			return fmt.Errorf("%s.endpoint is invalid", prefix)
		}
		switch exporter.Protocol {
		case "http/protobuf":
			parsed, err := url.ParseRequestURI(exporter.Endpoint)
			if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return fmt.Errorf("%s.endpoint must be an absolute http(s) URL without credentials or query", prefix)
			}
			if exporter.Insecure && parsed.Scheme != "http" {
				return fmt.Errorf("%s.insecure requires a plaintext http endpoint", prefix)
			}
		case "grpc":
			if _, _, splitErr := net.SplitHostPort(exporter.Endpoint); splitErr == nil {
				if strings.ContainsAny(exporter.Endpoint, "/?#") {
					return fmt.Errorf("%s.endpoint must be a gRPC host:port", prefix)
				}
			} else {
				parsed, err := url.Parse(exporter.Endpoint)
				if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
					return fmt.Errorf("%s.endpoint must be a gRPC authority or absolute http(s) URL without credentials or path", prefix)
				}
				if exporter.Insecure && parsed.Scheme != "http" {
					return fmt.Errorf("%s.insecure requires a plaintext http endpoint", prefix)
				}
			}
		}
		if err := validTelemetryText(prefix+".headers_file", exporter.HeadersFile); err != nil {
			return err
		}
		seenSignals := make(map[string]struct{}, len(exporter.Signals))
		for _, signal := range exporter.Signals {
			if _, ok := telemetrySignals[signal]; !ok {
				return fmt.Errorf("%s.signals contains unsupported signal %q", prefix, signal)
			}
			if _, exists := seenSignals[signal]; exists {
				return fmt.Errorf("%s.signals contains duplicate signal %q", prefix, signal)
			}
			seenSignals[signal] = struct{}{}
		}
	}
	return nil
}

type OAuthRegistration struct {
	ClientID         string   `json:"client_id"`
	ClientSecretFile string   `json:"client_secret_file,omitempty"`
	AuthorizationURL string   `json:"authorization_url"`
	TokenURL         string   `json:"token_url"`
	DeviceURL        string   `json:"device_url,omitempty"`
	RedirectURL      string   `json:"redirect_url"`
	Issuer           string   `json:"issuer,omitempty"`
	Scopes           []string `json:"scopes"`
}

const DefaultMaxConnsPerHost = 256
const MaximumMaxConnsPerHost = 65536

// TransportConfig bounds upstream connections per host within each isolated
// network-policy client. Zero inherits the safe default, never unlimited.
type TransportConfig struct {
	MaxConnsPerHost int `json:"max_conns_per_host,omitempty"`
}

func (c TransportConfig) Validate() error {
	if c.MaxConnsPerHost < 0 || c.MaxConnsPerHost > MaximumMaxConnsPerHost {
		return fmt.Errorf("transport.max_conns_per_host must be between 1 and %d, or 0 for the default", MaximumMaxConnsPerHost)
	}
	return nil
}

func (c TransportConfig) ConnectionLimit() int {
	if c.MaxConnsPerHost == 0 {
		return DefaultMaxConnsPerHost
	}
	return c.MaxConnsPerHost
}

func (c BootstrapConfig) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if c.DataDir == "" || c.Encryption.KeyFile == "" {
		return fmt.Errorf("data_dir and encryption.key_file must be explicit")
	}
	switch c.Mode {
	case "standalone":
		if c.Storage.SQLite.Path == "" || c.Storage.Postgres.DSNFile != "" {
			return fmt.Errorf("standalone requires only an explicit SQLite path")
		}
	case "cluster":
		if c.Storage.Postgres.DSNFile == "" || c.Storage.SQLite.Path != "" {
			return fmt.Errorf("cluster requires only an explicit PostgreSQL DSN file")
		}
	default:
		return fmt.Errorf("mode must be standalone or cluster")
	}
	if c.Listeners.Inference == "" || c.Listeners.Management == "" {
		return fmt.Errorf("both listeners must be configured")
	}
	if (c.OIDC.Issuer == "") != (c.OIDC.ClientID == "") {
		return fmt.Errorf("OIDC issuer and client_id must be configured together")
	}
	if err := c.Transport.Validate(); err != nil {
		return err
	}
	if err := c.Telemetry.Validate(); err != nil {
		return err
	}
	return nil
}
