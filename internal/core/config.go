package core

import (
	"encoding/json"
	"fmt"
	"io"
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

func DecodeBootstrap(r io.Reader) (BootstrapConfig, error) {
	var c BootstrapConfig
	c.Mode = "standalone"
	c.Listeners.Inference = ":8080"
	c.Listeners.Management = "127.0.0.1:8081"
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
	return nil
}
