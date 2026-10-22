package trustcheck

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"debug/buildinfo"
	"embed"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These flags are required by the container and release packaging gates, but
// optional for ordinary unit tests on hosts with unrelated system trust stores.
var (
	caBundle = flag.String("ca-bundle", "", "container CA bundle to inspect")
	artifact = flag.String("artifact", "", "release executable to inspect")
)

//go:embed testdata/*.pem testdata/chains/*.pem
var fixtures embed.FS

// Freeze certificate validity so fixture expiry cannot masquerade as distrust.
var verificationTime = time.Date(2026, time.October, 22, 16, 0, 0, 0, time.UTC)

func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for len(bytes.TrimSpace(data)) != 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid certificate PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
		data = rest
	}
	if len(certs) == 0 {
		return nil, errors.New("empty certificate bundle")
	}
	return certs, nil
}

func fixtureCertificates(t *testing.T, name string) []*x509.Certificate {
	t.Helper()
	data, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := parseCertificates(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return certs
}

func pool(certs []*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, cert := range certs {
		p.AddCert(cert)
	}
	return p
}

func chains(t *testing.T) map[string][]*x509.Certificate {
	t.Helper()
	entries, err := fixtures.ReadDir("testdata/chains")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 14 {
		t.Fatalf("expected all 14 incident test chains, found %d", len(entries))
	}
	result := make(map[string][]*x509.Certificate, len(entries))
	for _, entry := range entries {
		result[entry.Name()] = fixtureCertificates(t, "chains/"+entry.Name())
	}
	return result
}

func TestCompromisedChainsRejected(t *testing.T) {
	roots := fixtureCertificates(t, "affected-roots.pem")
	if len(roots) != 4 {
		t.Fatalf("expected four affected trust anchors, found %d", len(roots))
	}
	for name, chain := range chains(t) {
		t.Run(name, func(t *testing.T) {
			if len(chain) < 2 {
				t.Fatal("fixture must contain a leaf and intermediate")
			}
			for i, cert := range chain {
				if verificationTime.Before(cert.NotBefore) || verificationTime.After(cert.NotAfter) {
					t.Fatal("fixture is not valid at the fixed verification time")
				}
				parents := roots
				if i+1 < len(chain) {
					parents = chain[i+1 : i+2]
				}
				signed := false
				for _, parent := range parents {
					if cert.CheckSignatureFrom(parent) == nil {
						signed = true
						break
					}
				}
				if !signed {
					t.Fatal("fixture signature does not lead to an affected trust anchor")
				}
			}
			_, err := chain[0].Verify(x509.VerifyOptions{
				Roots:         pool(roots),
				Intermediates: pool(chain[1:]),
				DNSName:       strings.TrimSuffix(name, ".pem"),
				CurrentTime:   verificationTime,
			})
			var invalid x509.CertificateInvalidError
			if !errors.As(err, &invalid) || invalid.Reason != x509.NoValidChains {
				t.Fatalf("expected Go's compromised-key rejection, got %v", err)
			}
		})
	}
}

func TestValidChainAccepted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             verificationTime.Add(-time.Hour),
		NotAfter:              verificationTime.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    root.NotBefore,
		NotAfter:     root.NotAfter,
		DNSNames:     []string{"trust-control.test"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool([]*x509.Certificate{root}),
		DNSName:     "trust-control.test",
		CurrentTime: verificationTime,
	}); err != nil {
		t.Fatalf("valid control chain rejected: %v", err)
	}
}

func blockedKeys(t *testing.T) map[[sha256.Size]byte]struct{} {
	t.Helper()
	blocked := make(map[[sha256.Size]byte]struct{})
	add := func(certs []*x509.Certificate) {
		for _, cert := range certs {
			blocked[sha256.Sum256(cert.RawSubjectPublicKeyInfo)] = struct{}{}
		}
	}
	add(fixtureCertificates(t, "affected-roots.pem"))
	for _, chain := range chains(t) {
		add(chain[1:])
	}
	if len(blocked) != 10 {
		t.Fatalf("expected four root keys and six TLS intermediate keys, found %d", len(blocked))
	}
	return blocked
}

func checkBundle(data []byte, blocked map[[sha256.Size]byte]struct{}, control *x509.Certificate) error {
	certs, err := parseCertificates(data)
	if err != nil {
		return err
	}
	// Go's own key blocklist would mask a stale OS bundle if we only called
	// Verify. Inspect the anchors themselves to protect non-Go consumers too.
	for _, cert := range certs {
		if _, found := blocked[sha256.Sum256(cert.RawSubjectPublicKeyInfo)]; found {
			return fmt.Errorf("CA bundle still trusts an affected key: %s", cert.Subject)
		}
	}
	if _, err := control.Verify(x509.VerifyOptions{
		Roots:       pool(certs),
		CurrentTime: verificationTime,
	}); err != nil {
		return fmt.Errorf("unaffected ISRG Root X1 control is not trusted: %w", err)
	}
	return nil
}

func TestCABundle(t *testing.T) {
	if *caBundle == "" {
		t.Skip("container CA bundle not requested")
	}
	// Some OS stores contain legacy negative-serial roots trusted by non-Go
	// consumers. Parse them for this audit only; do not silently skip them.
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509negativeserial=1")
	data, err := os.ReadFile(*caBundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkBundle(data, blockedKeys(t), fixtureCertificates(t, "isrg-root-x1.pem")[0]); err != nil {
		t.Fatal(err)
	}
}

func TestBundleCheck(t *testing.T) {
	good, err := fixtures.ReadFile("testdata/isrg-root-x1.pem")
	if err != nil {
		t.Fatal(err)
	}
	control := fixtureCertificates(t, "isrg-root-x1.pem")[0]
	if got := fmt.Sprintf("%X", sha256.Sum256(control.Raw)); got != "96BCEC06264976F37460779ACF28C5A7CFE8A3C0AAE11A8FFCEE05C0BDDF08C6" {
		t.Fatal("ISRG Root X1 control fingerprint changed")
	}
	blocked := blockedKeys(t)
	if err := checkBundle(good, blocked, control); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("not PEM"), append(bytes.Clone(good), []byte("broken PEM")...)} {
		if err := checkBundle(data, blocked, control); err == nil {
			t.Fatal("invalid bundle accepted")
		}
	}
	leaf := fixtureCertificates(t, "chains/dv-g3b-emudhra.we-have-a-problem.com.pem")[0]
	withoutControl := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	if err := checkBundle(withoutControl, blocked, control); err == nil || !strings.Contains(err.Error(), "control is not trusted") {
		t.Fatalf("bundle missing the unaffected trust control accepted: %v", err)
	}
	badCerts := fixtureCertificates(t, "affected-roots.pem")
	for _, chain := range chains(t) {
		badCerts = append(badCerts, chain[1:]...)
	}
	for _, cert := range badCerts {
		data := append(bytes.Clone(good), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		if err := checkBundle(data, blocked, control); err == nil || !strings.Contains(err.Error(), "affected key") {
			t.Fatalf("affected key %s accepted: %v", cert.Subject, err)
		}
	}
}

func TestReleaseToolchain(t *testing.T) {
	if *artifact == "" {
		t.Skip("release executable not requested")
	}
	info, err := buildinfo.ReadFile(*artifact)
	if err != nil {
		t.Fatal(err)
	}
	if info.GoVersion != runtime.Version() {
		t.Fatalf("release uses %s, but certificate checks ran with %s", info.GoVersion, runtime.Version())
	}
}
