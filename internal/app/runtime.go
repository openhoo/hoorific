package app

import (
	"context"
	"fmt"
	"io"
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
	key := c.TenantID + "\x00" + c.ID // NUL separator: tenant/connection IDs may contain "/"
	// Critical section must stay bounded: holders may block only on the capped
	// regular-file read in readCredentialFile, never on FIFOs, devices, or
	// unbounded files, so queued cloud leases cannot be wedged indefinitely.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		raw, err = readCredentialFile(ctx, c.Settings["credential_file"])
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
		nativeCodex := c.ClientProfile != nil && c.ClientProfile.EmulatesCodex()
		p := transport.NetworkPolicy{AllowedHosts: []string{u.Hostname()}, AllowPrivate: c.Settings["allow_private"] == "true", AllowSameOriginRedirect: c.Settings["allow_same_origin_redirect"] == "true", DisableCompression: nativeCodex || (c.ClientProfile != nil && c.ClientProfile.PreservesClientHeaders()), NativeCodex: nativeCodex, NativeScope: c.TenantID + "\x00" + c.ID}
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

const credentialFileLimit = 4 << 20 // matches the Anthropic WIF identity token boundary

// readCredentialFile applies the same bounded regular-file semantics as
// configuredOAuthSecret: os.Stat rejects non-regular or oversized inputs
// before os.Open (a FIFO or device open would otherwise block while the
// global IdentitySource mutex is held), the fd re-stat narrows the
// stat-to-open swap window, and the read is capped at credentialFileLimit+1
// bytes. ctx is checked before any potentially blocking call.
func readCredentialFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("credential file path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > credentialFileLimit {
		return nil, fmt.Errorf("credential file must be a regular file no larger than %d MiB", credentialFileLimit>>20)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() || info.Size() > credentialFileLimit {
		return nil, fmt.Errorf("credential file must be a regular file no larger than %d MiB", credentialFileLimit>>20)
	}
	raw, err := io.ReadAll(io.LimitReader(file, credentialFileLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > credentialFileLimit {
		return nil, fmt.Errorf("credential file must be a regular file no larger than %d MiB", credentialFileLimit>>20)
	}
	return raw, nil
}
