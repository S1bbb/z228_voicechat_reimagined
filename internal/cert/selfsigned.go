// Package cert — самоподписанный сертификат для HTTPS (микрофон в браузере требует HTTPS не на localhost).
// Сертификат хранится в каталоге данных, чтобы браузер не просил подтверждать его после каждого перезапуска.
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// LoadOrCreate возвращает сертификат из dir; пересоздаёт, если его нет, он истекает
// или в нём не хватает каких-то из ips. При перевыпуске ключ сохраняется (если читается):
// десктоп-приложение пинит отпечаток открытого ключа (SPKI), и он не должен меняться.
func LoadOrCreate(dir string, ips []net.IP) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "selfsigned.crt")
	keyPath := filepath.Join(dir, "selfsigned.key")
	ips = append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, ips...)

	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && len(c.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour && coversAll(leaf, ips) {
			return c, nil
		}
	}

	key := loadKey(keyPath)
	if key == nil {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "z228 voice chat"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(397 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           dedupe(ips),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(certPath, certPEM, 0o644)
		_ = os.WriteFile(keyPath, keyPEM, 0o600)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// loadKey читает существующий ECDSA-ключ; nil — если файла нет или он испорчен.
func loadKey(path string) *ecdsa.PrivateKey {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		if ek, ok := k.(*ecdsa.PrivateKey); ok {
			return ek
		}
	}
	return nil
}

// SPKIPin — SHA-256 от SubjectPublicKeyInfo сертификата, base64url без «=».
// Так десктоп-приложение узнаёт «свой» сервер с самоподписанным сертификатом.
func SPKIPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func coversAll(leaf *x509.Certificate, ips []net.IP) bool {
	for _, ip := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			if have.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func dedupe(ips []net.IP) []net.IP {
	var out []net.IP
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		dup := false
		for _, o := range out {
			if o.Equal(ip) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, ip)
		}
	}
	return out
}
