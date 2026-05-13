package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// TLSOptions configures TLS for the relay Server. Use either Config +
// (CertFile, KeyFile) — Go's http.Server requires file paths even when
// you've already loaded the cert — or call SelfSignedTLS to materialize
// an in-memory self-signed cert to a temp directory.
type TLSOptions struct {
	Config   *tls.Config // optional; populated by SelfSignedTLS
	CertFile string
	KeyFile  string

	// fingerprint is populated by SelfSignedTLS so callers can log it.
	Fingerprint string

	// cleanup is set by SelfSignedTLS so callers can remove temp files.
	cleanup func() error
}

// Cleanup removes any temp files created by SelfSignedTLS. Safe to call
// on a nil receiver or when no cleanup is needed (no-op).
func (t *TLSOptions) Cleanup() error {
	if t == nil || t.cleanup == nil {
		return nil
	}
	return t.cleanup()
}

// LoadTLSFromFiles validates that the given PEM cert/key files exist and
// can be parsed, returning TLSOptions configured to use them.
func LoadTLSFromFiles(certFile, keyFile string) (*TLSOptions, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS cert: %w", err)
	}
	return &TLSOptions{
		Config:   &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		CertFile: certFile,
		KeyFile:  keyFile,
	}, nil
}

// SelfSignedTLS generates an ECDSA P-256 self-signed certificate valid
// for "localhost" and 127.0.0.1/::1 (plus optional extra hostnames),
// writes it to a temp directory, and returns TLSOptions pointing at the
// files. Call Cleanup() when done to remove the temp files.
//
// The returned TLSOptions.Fingerprint is the SHA-256 of the DER-encoded
// certificate, useful for logging so users can verify the cert out-of-band.
func SelfSignedTLS(extraHostnames ...string) (*TLSOptions, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gen key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("gen serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "aztunnel-relay"},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  parseIPs("127.0.0.1", "::1"),
		DNSNames:     append([]string{"localhost"}, extraHostnames...),
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create cert: %w", err)
	}
	fp := fingerprintHex(derBytes)

	dir, err := os.MkdirTemp("", "aztunnel-relay-tls-")
	if err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := writePEM(certPath, "CERTIFICATE", derBytes, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("reload cert: %w", err)
	}

	return &TLSOptions{
		Config:      &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		CertFile:    certPath,
		KeyFile:     keyPath,
		Fingerprint: fp,
		cleanup:     func() error { return os.RemoveAll(dir) },
	}, nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}
