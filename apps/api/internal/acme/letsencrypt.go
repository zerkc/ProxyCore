package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// LetsEncryptOptions configures ACME issuance.
type LetsEncryptOptions struct {
	Hostnames          []string
	DirectoryURL       string
	Email              string
	Challenge          string // "http-01" or "dns-01"
	KeyType            string // "rsa" or "ecdsa"
	PropagationSeconds int
	Http01             Http01Store
	Dns01              Dns01Adapter
}

// IssueLetsEncrypt obtains a certificate from an ACME directory.
func IssueLetsEncrypt(ctx context.Context, options LetsEncryptOptions) (Material, error) {
	names, err := normalizeHostnames(options.Hostnames)
	if err != nil {
		return Material{}, err
	}
	if options.Challenge == "http-01" {
		for _, name := range names {
			if strings.HasPrefix(name, "*.") {
				return Material{}, errors.New("Let's Encrypt HTTP-01 cannot issue wildcard certificates")
			}
		}
		if options.Http01 == nil {
			return Material{}, errors.New("HTTP-01 challenge store is required")
		}
	}
	if options.Challenge == "dns-01" && options.Dns01 == nil {
		return Material{}, errors.New("DNS-01 adapter is required")
	}
	if strings.TrimSpace(options.DirectoryURL) == "" {
		return Material{}, errors.New("ACME directory URL is required")
	}

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, err
	}
	client := &xacme.Client{Key: accountKey, DirectoryURL: options.DirectoryURL}
	account := &xacme.Account{}
	if options.Email != "" {
		account.Contact = []string{"mailto:" + options.Email}
	}
	if _, err := client.Register(ctx, account, xacme.AcceptTOS); err != nil &&
		!errors.Is(err, xacme.ErrAccountAlreadyExists) {
		return Material{}, err
	}

	order, err := client.AuthorizeOrder(ctx, xacme.DomainIDs(names...))
	if err != nil {
		return Material{}, err
	}

	if options.Challenge == "dns-01" {
		if err := authorizeDNSChallenges(ctx, client, order.AuthzURLs, options); err != nil {
			return Material{}, err
		}
	} else {
		for _, authzURL := range order.AuthzURLs {
			if err := authorizeChallenge(ctx, client, authzURL, options); err != nil {
				return Material{}, err
			}
		}
	}

	certKey, err := newCertificateKey(options.KeyType)
	if err != nil {
		return Material{}, err
	}
	csrTemplate := &x509.CertificateRequest{Subject: pkix.Name{CommonName: names[0]}}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			csrTemplate.IPAddresses = append(csrTemplate.IPAddresses, ip)
		} else {
			csrTemplate.DNSNames = append(csrTemplate.DNSNames, name)
		}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, certKey)
	if err != nil {
		return Material{}, err
	}
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return Material{}, err
	}

	var certPEM strings.Builder
	for _, block := range der {
		if err := pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: block}); err != nil {
			return Material{}, err
		}
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(certKey)
	if err != nil {
		return Material{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	expiresAt := time.Now().Add(60 * 24 * time.Hour)
	if len(der) > 0 {
		if leaf, err := x509.ParseCertificate(der[0]); err == nil {
			expiresAt = leaf.NotAfter
		}
	}
	return Material{
		CertificatePEM: certPEM.String(),
		PrivateKeyPEM:  string(keyPEM),
		ExpiresAt:      expiresAt,
	}, nil
}

type pendingDNSChallenge struct {
	authzURL   string
	challenge  *xacme.Challenge
	identifier string
	record     string
}

// authorizeDNSChallenges presents every DNS-01 value first (apex + wildcard share
// one TXT name), waits once for propagation, then accepts all authorizations.
func authorizeDNSChallenges(ctx context.Context, client *xacme.Client, authzURLs []string, options LetsEncryptOptions) error {
	pending := make([]pendingDNSChallenge, 0, len(authzURLs))
	cleared := map[string]bool{}

	for _, authzURL := range authzURLs {
		authz, challenge, err := loadChallenge(ctx, client, authzURL, "dns-01")
		if err != nil {
			return err
		}
		if authz == nil {
			continue
		}
		record, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return err
		}
		identifier := authz.Identifier.Value
		clearKey := dnsChallengeClearKey(identifier)
		if !cleared[clearKey] {
			if err := options.Dns01.CleanupAll(ctx, identifier); err != nil {
				return err
			}
			cleared[clearKey] = true
		}
		if err := options.Dns01.Present(ctx, identifier, record); err != nil {
			return err
		}
		pending = append(pending, pendingDNSChallenge{
			authzURL:   authzURL,
			challenge:  challenge,
			identifier: identifier,
			record:     record,
		})
	}
	defer func() {
		for _, item := range pending {
			_ = options.Dns01.Cleanup(ctx, item.identifier, item.record)
		}
	}()

	waitSeconds(options.PropagationSeconds)

	for _, item := range pending {
		if _, err := client.Accept(ctx, item.challenge); err != nil {
			return err
		}
		if _, err := client.WaitAuthorization(ctx, item.authzURL); err != nil {
			return err
		}
	}
	return nil
}

func dnsChallengeClearKey(identifier string) string {
	normalized := strings.ToLower(strings.TrimSpace(identifier))
	normalized = strings.TrimPrefix(normalized, "*.")
	return normalized
}

func authorizeChallenge(ctx context.Context, client *xacme.Client, authzURL string, options LetsEncryptOptions) error {
	authz, challenge, err := loadChallenge(ctx, client, authzURL, options.Challenge)
	if err != nil {
		return err
	}
	if authz == nil {
		return nil
	}

	identifier := authz.Identifier.Value
	switch options.Challenge {
	case "http-01":
		response, err := client.HTTP01ChallengeResponse(challenge.Token)
		if err != nil {
			return err
		}
		options.Http01.Put(challenge.Token, response, nil)
		defer options.Http01.Remove(challenge.Token)
	case "dns-01":
		record, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return err
		}
		if err := options.Dns01.CleanupAll(ctx, identifier); err != nil {
			return err
		}
		if err := options.Dns01.Present(ctx, identifier, record); err != nil {
			return err
		}
		defer func() { _ = options.Dns01.Cleanup(ctx, identifier, record) }()
		waitSeconds(options.PropagationSeconds)
	}

	if _, err := client.Accept(ctx, challenge); err != nil {
		return err
	}
	if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
		return err
	}
	return nil
}

func loadChallenge(ctx context.Context, client *xacme.Client, authzURL, challengeType string) (*xacme.Authorization, *xacme.Challenge, error) {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return nil, nil, err
	}
	if authz.Status == xacme.StatusValid {
		return nil, nil, nil
	}
	for _, candidate := range authz.Challenges {
		if candidate.Type == challengeType {
			return authz, candidate, nil
		}
	}
	return nil, nil, errors.New("ACME server did not offer the requested challenge: " + challengeType)
}

func waitSeconds(seconds int) {
	if seconds <= 0 {
		return
	}
	if seconds > 600 {
		seconds = 600
	}
	time.Sleep(time.Duration(seconds) * time.Second)
}

func newCertificateKey(keyType string) (crypto.Signer, error) {
	if keyType == "ecdsa" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	return rsa.GenerateKey(rand.Reader, 2048)
}
