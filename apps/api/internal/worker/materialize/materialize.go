// Package materialize builds the secret-bearing candidate files (TLS
// certificates, private keys and Basic Auth htpasswd entries) that the worker
// places beside an nginx candidate. It ports packages/certificates
// materialize.ts and the materialize half of basic-auth.ts, plus
// formatBasicAuthFileLine from packages/crypto.
//
// The functions only return relative path -> contents maps; the caller owns
// writing them to disk and therefore the file modes.
package materialize

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// SecretReader resolves a stored secret. A secret that does not exist yields
// ("", nil), matching secrets.SQLiteStore.Get.
type SecretReader interface {
	Get(ctx context.Context, id string) (string, error)
}

type validationError string

func (e validationError) Error() string { return string(e) }

var basicAuthUsername = regexp.MustCompile(`^[A-Za-z0-9._@+=-]{1,64}$`)

// FormatBasicAuthFileLine returns the htpasswd line "<username>:<hash>\n".
// The hash must be an nginx {SHA} hash; nothing else is accepted.
func FormatBasicAuthFileLine(username, passwordHash string) (string, error) {
	if !basicAuthUsername.MatchString(username) {
		return "", validationError("Basic Auth username is invalid")
	}
	// TS compares UTF-16 length; count code units to stay identical.
	if !strings.HasPrefix(passwordHash, "{SHA}") || len(utf16.Encode([]rune(passwordHash))) <= 6 {
		return "", validationError("Basic Auth password hash is invalid")
	}
	return username + ":" + passwordHash + "\n", nil
}

// BasicAuthFiles returns "basic-auth/<secretId>" -> htpasswd line for every
// record with Basic Auth configured (enabled state is not considered, like TS).
func BasicAuthFiles(ctx context.Context, records []domain.DNSRecord, secrets SecretReader) (map[string]string, error) {
	files := map[string]string{}
	for _, record := range records {
		basicAuth, _ := record.Proxy["basicAuth"].(map[string]any)
		if basicAuth == nil {
			continue
		}
		username, _ := basicAuth["username"].(string)
		secretID, _ := basicAuth["passwordSecretId"].(string)
		hash, err := secrets.Get(ctx, secretID)
		if err != nil {
			return nil, err
		}
		if hash == "" {
			return nil, validationError("Basic Auth secret not found: " + secretID)
		}
		line, err := FormatBasicAuthFileLine(username, hash)
		if err != nil {
			return nil, err
		}
		files["basic-auth/"+secretID] = line
	}
	return files, nil
}

// CertificateFiles returns "certs/<id>.crt" and "certs/<id>.key" for every
// certificate referenced by an enabled, proxied, TLS-enabled record. Contents
// always end with a newline.
func CertificateFiles(ctx context.Context, records []domain.DNSRecord, certificates []domain.CertificateStatus, secrets SecretReader) (map[string]string, error) {
	var needed []string
	seen := map[string]bool{}
	for _, record := range records {
		if !record.Enabled || !record.Proxied {
			continue
		}
		if tls, _ := record.Proxy["tlsEnabled"].(bool); !tls {
			continue
		}
		id, _ := record.Proxy["certificateId"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		needed = append(needed, id)
	}
	files := map[string]string{}
	for _, id := range needed {
		var cert *domain.CertificateStatus
		for i := range certificates {
			if certificates[i].ID == id {
				cert = &certificates[i]
				break
			}
		}
		if cert == nil {
			return nil, validationError("Certificate not found: " + id)
		}
		if cert.CertificatePEM == "" {
			return nil, validationError("Certificate PEM is missing: " + id)
		}
		if cert.SecretID == "" {
			return nil, validationError("Certificate private key is missing: " + id)
		}
		key, err := secrets.Get(ctx, cert.SecretID)
		if err != nil {
			return nil, err
		}
		if key == "" {
			return nil, validationError("Certificate key secret not found: " + cert.SecretID)
		}
		files["certs/"+id+".crt"] = ensureTrailingNewline(cert.CertificatePEM)
		files["certs/"+id+".key"] = ensureTrailingNewline(key)
	}
	return files, nil
}

func ensureTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}
