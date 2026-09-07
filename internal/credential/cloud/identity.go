// Package cloud contains explicitly enabled official workload identity leases.
package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Azure struct{ credential azcore.TokenCredential }

func NewAzure(c azcore.TokenCredential) *Azure { return &Azure{credential: c} }
func NewAzureManagedIdentity(clientID string) (*Azure, error) {
	options := &azidentity.ManagedIdentityCredentialOptions{}
	if clientID != "" {
		options.ID = azidentity.ClientID(clientID)
	}
	c, e := azidentity.NewManagedIdentityCredential(options)
	if e != nil {
		return nil, e
	}
	return NewAzure(c), nil
}
func NewAzureWorkloadIdentity(tenantID, clientID, tokenFile string) (*Azure, error) {
	if tenantID == "" || clientID == "" || tokenFile == "" {
		return nil, errors.New("Azure workload identity requires explicit tenant, client and token file")
	}
	c, e := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{TenantID: tenantID, ClientID: clientID, TokenFilePath: tokenFile})
	if e != nil {
		return nil, e
	}
	return NewAzure(c), nil
}
func (a *Azure) Authorize(ctx context.Context, r *http.Request) error {
	if a.credential == nil {
		return errors.New("Azure credential is missing")
	}
	t, e := a.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://cognitiveservices.azure.com/.default"}})
	if e != nil {
		return errors.New("Azure token acquisition failed")
	}
	r.Header.Set("Authorization", "Bearer "+t.Token)
	return nil
}

type GoogleConfig struct {
	CredentialsJSON []byte
	EnableADC       bool
}
type Google struct{ source oauth2.TokenSource }

func NewGoogle(ctx context.Context, c GoogleConfig) (*Google, error) {
	var creds *google.Credentials
	var err error
	if len(c.CredentialsJSON) > 0 {
		creds, err = google.CredentialsFromJSON(ctx, c.CredentialsJSON, "https://www.googleapis.com/auth/cloud-platform")
	} else if c.EnableADC {
		creds, err = google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	} else {
		return nil, errors.New("Google identity requires explicit credentials or ADC opt-in")
	}
	if err != nil {
		return nil, errors.New("Google credential configuration failed")
	}
	return &Google{source: oauth2.ReuseTokenSource(nil, creds.TokenSource)}, nil
}
func (g *Google) Authorize(ctx context.Context, r *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t, e := g.source.Token()
	if e != nil {
		return errors.New("Google token acquisition failed")
	}
	r.Header.Set("Authorization", "Bearer "+t.AccessToken)
	return nil
}

type AWSConfig struct {
	Region, AccessKeyID, SecretAccessKey, SessionToken, RoleARN, ExternalID string
	EnableChain                                                             bool
	HTTPClient                                                              aws.HTTPClient
}
type AWS struct {
	Credentials aws.CredentialsProvider
	Region      string
	signer      *v4.Signer
}

func NewAWS(ctx context.Context, c AWSConfig) (*AWS, error) {
	if c.Region == "" {
		return nil, errors.New("AWS region must be explicit")
	}
	opts := []func(*config.LoadOptions) error{config.WithRegion(c.Region), config.WithRetryMaxAttempts(1)}
	if c.HTTPClient != nil {
		opts = append(opts, config.WithHTTPClient(c.HTTPClient))
	}
	if c.AccessKeyID != "" || c.SecretAccessKey != "" {
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return nil, errors.New("AWS static credentials are incomplete")
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)))
	} else if !c.EnableChain {
		return nil, errors.New("AWS credential chain requires explicit opt-in")
	}
	cfg, e := config.LoadDefaultConfig(ctx, opts...)
	if e != nil {
		return nil, errors.New("AWS credential configuration failed")
	}
	provider := cfg.Credentials
	if c.RoleARN != "" {
		provider = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), c.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			if c.ExternalID != "" {
				o.ExternalID = aws.String(c.ExternalID)
			}
		}))
	}
	return &AWS{Credentials: provider, Region: c.Region, signer: v4.NewSigner()}, nil
}
func (a *AWS) Authorize(ctx context.Context, r *http.Request) error {
	h := sha256.New()
	if r.Body != nil && r.Body != http.NoBody {
		if r.GetBody == nil {
			return errors.New("SigV4 requires a bounded replayable body")
		}
		b, e := r.GetBody()
		if e != nil {
			return e
		}
		_, e = io.Copy(h, b)
		ce := b.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	c, e := a.Credentials.Retrieve(ctx)
	if e != nil {
		return errors.New("AWS credential acquisition failed")
	}
	return a.signer.SignHTTP(ctx, c, r, hex.EncodeToString(h.Sum(nil)), "bedrock", a.Region, time.Now())
}
func (a *AWS) AWSCredentials() aws.CredentialsProvider { return a.Credentials }
func (a *AWS) AWSRegion() string                       { return a.Region }

type WIFConfig struct {
	FederationRuleID, OrganizationID, ServiceAccountID, WorkspaceID, IdentityTokenFile, Endpoint string
	HTTPClient                                                                                   *http.Client
	RefreshBefore                                                                                time.Duration
}
type WIF struct {
	cfg     WIFConfig
	client  *http.Client
	mu      sync.Mutex
	refresh sync.Mutex
	token   string
	expiry  time.Time
}
type wifResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func NewWIF(c WIFConfig) (*WIF, error) {
	if c.FederationRuleID == "" || c.OrganizationID == "" || c.ServiceAccountID == "" || c.IdentityTokenFile == "" {
		return nil, errors.New("Anthropic WIF requires rule, organization, service account and token file")
	}
	if c.Endpoint == "" {
		c.Endpoint = "https://api.anthropic.com/v1/oauth/token"
	}
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("Anthropic WIF endpoint must be HTTPS")
	}
	if c.RefreshBefore <= 0 {
		c.RefreshBefore = 30 * time.Second
	}
	cl := c.HTTPClient
	if cl == nil {
		cl = http.DefaultClient
	}
	return &WIF{cfg: c, client: cl}, nil
}
func (w *WIF) Authorize(ctx context.Context, r *http.Request) error {
	t, e := w.accessToken(ctx)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+t)
	return nil
}
func (w *WIF) accessToken(ctx context.Context) (string, error) {
	w.mu.Lock()
	if w.token != "" && time.Now().Add(w.cfg.RefreshBefore).Before(w.expiry) {
		t := w.token
		w.mu.Unlock()
		return t, nil
	}
	w.mu.Unlock()
	w.refresh.Lock()
	defer w.refresh.Unlock()
	w.mu.Lock()
	if w.token != "" && time.Now().Add(w.cfg.RefreshBefore).Before(w.expiry) {
		t := w.token
		w.mu.Unlock()
		return t, nil
	}
	w.mu.Unlock()
	raw, e := os.ReadFile(w.cfg.IdentityTokenFile)
	if e != nil {
		return "", errors.New("Anthropic WIF identity token read failed")
	}
	if len(raw) == 0 || len(raw) > 4<<20 {
		return "", errors.New("Anthropic WIF identity token has invalid size")
	}
	payload := map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": strings.TrimSpace(string(raw)), "federation_rule_id": w.cfg.FederationRuleID, "organization_id": w.cfg.OrganizationID, "service_account_id": w.cfg.ServiceAccountID}
	if w.cfg.WorkspaceID != "" {
		payload["workspace_id"] = w.cfg.WorkspaceID
	}
	body, e := json.Marshal(payload)
	if e != nil {
		return "", errors.New("Anthropic WIF request encoding failed")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.Endpoint, strings.NewReader(string(body)))
	if e != nil {
		return "", errors.New("Anthropic WIF request creation failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := w.client.Do(req)
	if e != nil {
		return "", errors.New("Anthropic WIF exchange failed")
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return "", errors.New("Anthropic WIF response read failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Anthropic WIF exchange returned status %d", resp.StatusCode)
	}
	var out wifResponse
	if json.Unmarshal(data, &out) != nil || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("Anthropic WIF response missing access token")
	}
	w.mu.Lock()
	w.token = out.AccessToken
	w.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	t := w.token
	w.mu.Unlock()
	return t, nil
}
