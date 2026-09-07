package app

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"hoorific/internal/core"
	"hoorific/internal/credential/cloud"
	"hoorific/internal/transport"
)

type IdentitySource struct {
	Stored core.CredentialSource
	mu     sync.Mutex
	leases map[string]cachedLease
}
type cachedLease struct {
	version int64
	lease   core.CredentialLease
}

func NewIdentitySource(stored core.CredentialSource) *IdentitySource {
	return &IdentitySource{Stored: stored, leases: map[string]cachedLease{}}
}
func (s *IdentitySource) Lease(ctx context.Context, c core.Connection) (core.CredentialLease, error) {
	mode := c.Settings["auth_mode"]
	if mode == "" || mode == "api_key" || mode == "oauth" || mode == "keyless" {
		return s.Stored.Lease(ctx, c)
	}
	key := c.TenantID + "/" + c.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[key]; ok && l.version == c.Version {
		return l.lease, nil
	}
	var lease core.CredentialLease
	var err error
	switch mode {
	case "google_adc":
		lease, err = cloud.NewGoogle(ctx, cloud.GoogleConfig{EnableADC: true})
	case "google_service_account":
		var raw []byte
		raw, err = os.ReadFile(c.Settings["credential_file"])
		if err == nil {
			lease, err = cloud.NewGoogle(ctx, cloud.GoogleConfig{CredentialsJSON: raw})
		}
	case "aws_chain":
		lease, err = cloud.NewAWS(ctx, cloud.AWSConfig{Region: c.Region, EnableChain: true, RoleARN: c.Settings["role_arn"], ExternalID: c.Settings["external_id"]})
	case "azure_managed_identity":
		lease, err = cloud.NewAzureManagedIdentity(c.Settings["client_id"])
	case "azure_workload_identity":
		lease, err = cloud.NewAzureWorkloadIdentity(c.Settings["tenant_id"], c.Settings["client_id"], c.Settings["token_file"])
	case "anthropic_wif":
		lease, err = cloud.NewWIF(cloud.WIFConfig{FederationRuleID: c.Settings["federation_rule_id"], OrganizationID: c.Settings["organization_id"], ServiceAccountID: c.Settings["service_account_id"], WorkspaceID: c.Settings["workspace_id"], IdentityTokenFile: c.Settings["token_file"]})
	default:
		return nil, fmt.Errorf("unknown explicit authentication strategy")
	}
	if err != nil {
		return nil, err
	}
	s.leases[key] = cachedLease{version: c.Version, lease: lease}
	return lease, nil
}
func ClientFactory(pool *transport.Pool) func(core.Connection) (*http.Client, error) {
	return func(c core.Connection) (*http.Client, error) {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("connection requires explicit upstream base URL")
		}
		p := transport.NetworkPolicy{AllowedHosts: []string{u.Hostname()}, AllowPrivate: c.Settings["allow_private"] == "true", AllowSameOriginRedirect: c.Settings["allow_same_origin_redirect"] == "true"}
		for _, v := range strings.Split(c.Settings["allowed_cidrs"], ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			prefix, e := netip.ParsePrefix(v)
			if e != nil {
				return nil, fmt.Errorf("invalid egress CIDR")
			}
			p.AllowedCIDRs = append(p.AllowedCIDRs, prefix)
		}
		if p.AllowPrivate && len(p.AllowedCIDRs) == 0 {
			return nil, fmt.Errorf("private connections require an explicit CIDR allowlist")
		}
		return pool.Client(p)
	}
}
