package aadssh

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// certKeyType is the OpenSSH public key type prefix for an RSA certificate.
const certKeyType = "ssh-rsa-cert-v01@openssh.com"

// writeCert writes the certificate body returned by Entra ID as an OpenSSH
// certificate file ("ssh-rsa-cert-v01@openssh.com <body>") with 0644
// permissions, creating parent directories as needed. The write is atomic so a
// concurrent SSH connection never reads a half-written certificate.
func writeCert(path, body string) error {
	contents := certKeyType + " " + strings.TrimSpace(body) + "\n"
	if err := writeFileAtomic(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write certificate: %w", err)
	}
	return nil
}

// certStillValid reports whether cert is valid at now and remains valid for at
// least skew beyond now. A certificate whose ValidBefore is infinite is always
// valid once it has started.
func certStillValid(cert *ssh.Certificate, now time.Time, skew time.Duration) bool {
	unix := now.Unix()
	if unix < 0 {
		return false
	}
	if cert.ValidAfter > uint64(unix) {
		return false
	}
	if cert.ValidBefore == ssh.CertTimeInfinity {
		return true
	}
	deadline := now.Add(skew).Unix()
	if deadline < 0 {
		return false
	}
	return cert.ValidBefore > uint64(deadline)
}

func parseCertificate(data []byte) (*ssh.Certificate, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse SSH certificate: %w", err)
	}
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("SSH public key is not a certificate")
	}
	if cert.CertType != ssh.UserCert {
		return nil, fmt.Errorf("SSH certificate is not a user certificate")
	}
	return cert, nil
}

func parseCertificateBody(body string) (*ssh.Certificate, error) {
	data := []byte(certKeyType + " " + strings.TrimSpace(body) + "\n")
	return parseCertificate(data)
}

func readCertificate(path string) (*ssh.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCertificate(data)
}

func certificateUsername(cert *ssh.Certificate) (string, error) {
	if len(cert.ValidPrincipals) == 0 {
		return "", fmt.Errorf("certificate has no principals")
	}
	return strings.ToLower(cert.ValidPrincipals[0]), nil
}

func certificateMatchesPublicKey(cert *ssh.Certificate, expected ssh.PublicKey) bool {
	return bytes.Equal(cert.Key.Marshal(), expected.Marshal())
}

func validateCertificateBody(body string, expectedKey ssh.PublicKey, requestedUser string, now time.Time) (string, error) {
	cert, err := parseCertificateBody(body)
	if err != nil {
		return "", err
	}
	if !certificateMatchesPublicKey(cert, expectedKey) {
		return "", fmt.Errorf("minted certificate does not match the requested private key")
	}
	user, err := certificateUsername(cert)
	if err != nil {
		return "", err
	}
	if requestedUser != "" && !strings.EqualFold(user, requestedUser) {
		return "", fmt.Errorf("minted certificate principal %q does not match requested user %q (the token cache may hold a different account)", user, requestedUser)
	}

	if !certStillValid(cert, now, 0) {
		return "", fmt.Errorf("minted certificate is not currently valid")
	}
	return user, nil
}
