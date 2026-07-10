package aadssh

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// certKeyType is the OpenSSH public key type prefix for an RSA certificate.
const certKeyType = "ssh-rsa-cert-v01@openssh.com"

// sshKeygenTimeLayout matches the timestamps printed by `ssh-keygen -L`
// (e.g. "2024-06-01T12:00:00"), which are rendered in local time.
const sshKeygenTimeLayout = "2006-01-02T15:04:05"

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

// certInfo describes the parts of an OpenSSH certificate that this tool needs.
type certInfo struct {
	principals []string
	validFrom  time.Time
	validTo    time.Time
	forever    bool
}

// inspectCert runs `ssh-keygen -L -f <path>` and parses the principals and
// validity window. keygen is the ssh-keygen executable to invoke.
func inspectCert(keygen, path string) (*certInfo, error) {
	out, err := exec.Command(keygen, "-L", "-f", path).CombinedOutput()
	if err != nil {
		if diagnostic := strings.TrimSpace(string(out)); diagnostic != "" {
			return nil, fmt.Errorf("ssh-keygen -L %s: %w: %s", path, err, diagnostic)
		}
		return nil, fmt.Errorf("ssh-keygen -L %s: %w", path, err)
	}
	return parseCertInfo(string(out))
}

// parseCertInfo extracts principals and validity from `ssh-keygen -L` output.
//
// The relevant sections look like:
//
//	Valid: from 2024-06-01T12:00:00 to 2024-06-01T13:00:00
//	Principals:
//	        alice@contoso.com
//	Critical Options: (none)
//
// Principals are the more deeply indented lines following "Principals:", up to
// the next sibling section. This preserves free-form principals containing ":".
func parseCertInfo(output string) (*certInfo, error) {
	info := &certInfo{}
	lines := strings.Split(output, "\n")
	inPrincipals := false
	principalsIndent := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if inPrincipals {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent > principalsIndent {
				info.principals = append(info.principals, trimmed)
				continue
			}
			inPrincipals = false
		}
		switch {
		case strings.HasPrefix(trimmed, "Valid:"):
			info.parseValid(trimmed)
		case strings.HasPrefix(trimmed, "Principals:"):
			inPrincipals = true
			principalsIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			// Handle "Principals: (none)" or inline values on the same line.
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Principals:"))
			if rest != "" && rest != "(none)" {
				info.principals = append(info.principals, rest)
			}
		}
	}
	return info, nil
}

// parseValid interprets a "Valid: ..." line, setting validTo/forever.
func (info *certInfo) parseValid(line string) {
	// Examples:
	//   "Valid: forever"
	//   "Valid: from 2024-06-01T12:00:00 to 2024-06-01T13:00:00"
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[1] == "forever" {
		info.forever = true
		return
	}
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "from":
			if t, err := time.ParseInLocation(sshKeygenTimeLayout, fields[i+1], time.Local); err == nil {
				info.validFrom = t
			}
		case "to":
			if t, err := time.ParseInLocation(sshKeygenTimeLayout, fields[i+1], time.Local); err == nil {
				info.validTo = t
			}
		}
	}
}

// username returns the login name derived from the certificate: the first
// principal, lowercased (matching the Azure CLI behavior).
func (info *certInfo) username() (string, error) {
	if len(info.principals) == 0 {
		return "", fmt.Errorf("certificate has no principals")
	}
	return strings.ToLower(info.principals[0]), nil
}

// stillValid reports whether the certificate remains valid for at least skew
// beyond now.
func (info *certInfo) stillValid(now time.Time, skew time.Duration) bool {
	if info.forever {
		return true
	}
	if info.validFrom.IsZero() || info.validTo.IsZero() || info.validFrom.After(now) {
		return false
	}
	return info.validTo.After(now.Add(skew))
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

	unix := now.Unix()
	if unix < 0 {
		return "", fmt.Errorf("cannot validate certificate before the Unix epoch")
	}
	validAt := uint64(unix)
	if cert.ValidAfter > validAt || (cert.ValidBefore != ssh.CertTimeInfinity && cert.ValidBefore <= validAt) {
		return "", fmt.Errorf("minted certificate is not currently valid")
	}
	return user, nil
}
