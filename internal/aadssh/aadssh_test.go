package aadssh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestDeriveKeyMaterialEncoding(t *testing.T) {
	// A deterministic RSA key (small but valid structure) is enough to exercise
	// the SSH wire parsing and base64url encoding. We construct one from fixed
	// primes so the test is reproducible.
	priv := testKey(t)

	km, err := deriveKeyMaterial(priv)
	if err != nil {
		t.Fatalf("deriveKeyMaterial: %v", err)
	}

	// Exponent 65537 encodes to the SSH mpint 0x010001 -> urlsafe base64 "AQAB".
	if km.exponent != "AQAB" {
		t.Errorf("exponent = %q, want %q", km.exponent, "AQAB")
	}

	// Modulus and exponent must be valid standard urlsafe base64 (with padding).
	if _, err := base64.URLEncoding.DecodeString(km.modulus); err != nil {
		t.Errorf("modulus not urlsafe base64: %v", err)
	}
	if _, err := base64.URLEncoding.DecodeString(km.exponent); err != nil {
		t.Errorf("exponent not urlsafe base64: %v", err)
	}

	// req_cnf must be JSON with the expected fields, and n/e must match.
	var jwk struct {
		Kty, N, E, Kid string
	}
	if err := json.Unmarshal([]byte(km.reqCnf), &jwk); err != nil {
		t.Fatalf("req_cnf not valid JSON: %v", err)
	}
	if jwk.Kty != "RSA" {
		t.Errorf("kty = %q, want RSA", jwk.Kty)
	}
	if jwk.N != km.modulus || jwk.E != km.exponent {
		t.Errorf("jwk n/e do not match derived modulus/exponent")
	}
	if jwk.Kid != km.keyID {
		t.Errorf("jwk kid = %q, want %q", jwk.Kid, km.keyID)
	}

	// key_id is a hex sha256 (64 hex chars) of modulus||exponent.
	if len(km.keyID) != 64 {
		t.Errorf("keyID length = %d, want 64", len(km.keyID))
	}
}

func TestKeyIDDeterministic(t *testing.T) {
	priv := testKey(t)
	a, err := deriveKeyMaterial(priv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := deriveKeyMaterial(priv)
	if err != nil {
		t.Fatal(err)
	}
	if a.keyID != b.keyID {
		t.Errorf("keyID not deterministic: %q vs %q", a.keyID, b.keyID)
	}
	if a.modulus != b.modulus {
		t.Errorf("modulus not deterministic")
	}
}

func TestReadPrivateKeyFormats(t *testing.T) {
	priv := testKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS#8 key: %v", err)
	}
	openSSH, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal OpenSSH key: %v", err)
	}

	formats := map[string][]byte{
		"PKCS#1 PEM": pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(priv),
		}),
		"PKCS#8 PEM": pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: pkcs8,
		}),
		"OpenSSH": pem.EncodeToMemory(openSSH),
	}
	for name, data := range formats {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "id_rsa")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write private key: %v", err)
			}
			got, err := readPrivateKey(path)
			if err != nil {
				t.Fatalf("readPrivateKey: %v", err)
			}
			if got.N.Cmp(priv.N) != 0 || got.E != priv.E || got.D.Cmp(priv.D) != 0 {
				t.Fatal("parsed private key does not match input")
			}
		})
	}
}

func TestReadPrivateKeyRejectsNonRSA(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ECDSA key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ecdsa")
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	_, err = readPrivateKey(path)
	if err == nil {
		t.Fatal("expected non-RSA key to be rejected")
	}
	if !strings.Contains(err.Error(), "RSA is required") {
		t.Fatalf("error = %q, want RSA requirement", err)
	}
}

// testKey returns a fixed RSA key parsed from known parameters so tests are
// deterministic and fast. It is only used to exercise key encoding.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// Repeating the fixed hex strings produces ~512-bit primes and a ~1024-bit
	// modulus without making the test depend on random key generation.
	pStr := "e00e8bc8b2e1a0b0d6c9f2a1b3c4d5e6f7081920a1b2c3d4e5f60718293a4b5c7"
	qStr := "f10f9cd9c3f2b1c1e7daf3b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f89"
	p, _ := new(big.Int).SetString(pStr+pStr, 16)
	q, _ := new(big.Int).SetString(qStr+qStr, 16)
	if !p.ProbablyPrime(20) {
		p = nextPrime(p)
	}
	if !q.ProbablyPrime(20) {
		q = nextPrime(q)
	}
	n := new(big.Int).Mul(p, q)
	e := 65537
	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: e},
		D:         big.NewInt(0),
		Primes:    []*big.Int{p, q},
	}
	// Compute D so the key is internally consistent enough for marshaling.
	phi := new(big.Int).Mul(new(big.Int).Sub(p, big.NewInt(1)), new(big.Int).Sub(q, big.NewInt(1)))
	d := new(big.Int).ModInverse(big.NewInt(int64(e)), phi)
	if d == nil {
		t.Fatal("could not compute D for test key")
	}
	priv.D = d
	priv.Precompute()
	return priv
}

func nextPrime(n *big.Int) *big.Int {
	c := new(big.Int).Set(n)
	if c.Bit(0) == 0 {
		c.Add(c, big.NewInt(1))
	}
	for !c.ProbablyPrime(20) {
		c.Add(c, big.NewInt(2))
	}
	return c
}

func TestParseCertInfoPrincipalsAndValidity(t *testing.T) {
	output := `/tmp/id-cert.pub:
        Type: ssh-rsa-cert-v01@openssh.com user certificate
        Public key: RSA-CERT SHA256:abcdef
        Signing CA: RSA SHA256:ghijkl
        Key ID: "key1"
        Serial: 0
        Valid: from 2024-06-01T12:00:00 to 2024-06-01T13:00:00
        Principals:
                alice@contoso.com
                alice
        Critical Options: (none)
        Extensions:
                permit-pty
`
	info, err := parseCertInfo(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.principals) != 2 || info.principals[0] != "alice@contoso.com" || info.principals[1] != "alice" {
		t.Errorf("principals = %v, want [alice@contoso.com alice]", info.principals)
	}
	user, err := info.username()
	if err != nil {
		t.Fatal(err)
	}
	if user != "alice@contoso.com" {
		t.Errorf("username = %q, want alice@contoso.com", user)
	}

	want := time.Date(2024, 6, 1, 13, 0, 0, 0, time.Local)
	if !info.validTo.Equal(want) {
		t.Errorf("validTo = %v, want %v", info.validTo, want)
	}
	wantFrom := time.Date(2024, 6, 1, 12, 0, 0, 0, time.Local)
	if !info.validFrom.Equal(wantFrom) {
		t.Errorf("validFrom = %v, want %v", info.validFrom, wantFrom)
	}
}

func TestParseCertInfoForever(t *testing.T) {
	info, err := parseCertInfo("        Valid: forever\n        Principals:\n                root\n")
	if err != nil {
		t.Fatal(err)
	}
	if !info.forever {
		t.Error("expected forever = true")
	}
	if !info.stillValid(time.Now(), time.Hour) {
		t.Error("forever cert should always be valid")
	}
}

func TestParseCertInfoPrincipalContainingColon(t *testing.T) {
	output := `        Principals:
                alice:administrator
        Critical Options: (none)
        Extensions:
                permit-pty
`
	info, err := parseCertInfo(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.principals) != 1 || info.principals[0] != "alice:administrator" {
		t.Fatalf("principals = %v, want [alice:administrator]", info.principals)
	}
}

func TestStillValid(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.Local)
	info := &certInfo{validFrom: now.Add(-time.Minute), validTo: now.Add(10 * time.Minute)}
	if !info.stillValid(now, 5*time.Minute) {
		t.Error("cert valid for 10m should pass a 5m skew")
	}
	if info.stillValid(now, 15*time.Minute) {
		t.Error("cert valid for 10m should fail a 15m skew")
	}
	future := &certInfo{validFrom: now.Add(time.Minute), validTo: now.Add(time.Hour)}
	if future.stillValid(now, time.Minute) {
		t.Error("not-yet-valid cert should not be reused")
	}
	empty := &certInfo{}
	if empty.stillValid(now, time.Minute) {
		t.Error("cert with no validTo should not be valid")
	}
}

func TestEnsureCertRejectsNegativeMinValidity(t *testing.T) {
	_, err := EnsureCert(t.Context(), Options{
		Identity:    "unused",
		MinValidity: -time.Minute,
	})
	if err == nil {
		t.Fatal("expected negative MinValidity to fail")
	}
}
