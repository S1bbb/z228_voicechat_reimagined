package cert

import (
	"crypto/x509"
	"net"
	"testing"
)

// При перевыпуске (новый IP в SAN) ключ должен остаться прежним — иначе пин в десктопе сломается.
func TestReissueKeepsKey(t *testing.T) {
	dir := t.TempDir()
	c1, err := LoadOrCreate(dir, []net.IP{net.ParseIP("10.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreate(dir, []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	l1, _ := x509.ParseCertificate(c1.Certificate[0])
	l2, _ := x509.ParseCertificate(c2.Certificate[0])
	if l1.SerialNumber.Cmp(l2.SerialNumber) == 0 {
		t.Fatal("сертификат не перевыпущен")
	}
	if SPKIPin(l1) != SPKIPin(l2) {
		t.Fatalf("ключ сменился: %s → %s", SPKIPin(l1), SPKIPin(l2))
	}
	if len(SPKIPin(l1)) != 43 {
		t.Fatalf("неожиданная длина пина: %q", SPKIPin(l1))
	}
}
