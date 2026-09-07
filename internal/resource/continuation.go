package resource

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type continuationPayload struct{ URL, UploadID string }

func validContinuationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && continuationSchemeAllowed(u) && u.Host != "" && u.User == nil && u.Path != "" && !strings.Contains(u.Path, "..") && !strings.Contains(u.Path, "\\")
}

// HTTP is limited to literal loopback endpoints. Sealing still requires an
// exact descriptor-approved origin; outbound network policy remains mandatory.
func continuationSchemeAllowed(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.IsLoopback()
}
func continuationAAD(c Continuation) []byte {
	metadataHash := sha256.Sum256(c.Metadata)
	return []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%x", c.TenantID, c.ConnectionID, c.AccountID, c.ExpiresAt.UnixNano(), metadataHash))
}

// SealContinuation authenticates descriptor metadata alongside the approved URL.
// Populate Metadata before sealing. The plaintext URL is removed before persistence.
func SealContinuation(key []byte, c *Continuation, origins []string) error {
	if c == nil {
		return errors.New("nil continuation")
	}
	if !c.ExpiresAt.After(time.Now()) || !AllowedContinuationOrigin(c.UpstreamURL, origins) || !validContinuationURL(c.UpstreamURL) {
		return errors.New("continuation URL not allowed")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(continuationPayload{URL: c.UpstreamURL, UploadID: c.UploadID})
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	c.Ciphertext = append(nonce, gcm.Seal(nil, nonce, plain, continuationAAD(*c))...)
	c.UpstreamURL = ""
	return nil
}
func OpenContinuation(key []byte, c Continuation, now time.Time) (string, string, error) {
	if !c.ExpiresAt.After(now) {
		return "", "", errors.New("continuation expired")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	if len(c.Ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return "", "", errors.New("continuation ciphertext invalid")
	}
	nonce := c.Ciphertext[:gcm.NonceSize()]
	plain, err := gcm.Open(nil, nonce, c.Ciphertext[gcm.NonceSize():], continuationAAD(c))
	if err != nil {
		return "", "", errors.New("continuation authentication failed")
	}
	var p continuationPayload
	if err = json.Unmarshal(plain, &p); err != nil || !validContinuationURL(p.URL) {
		return "", "", errors.New("continuation payload invalid")
	}
	return p.URL, p.UploadID, nil
}
