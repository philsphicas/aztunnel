// Package aadssh implements automatic acquisition of short-lived OpenSSH
// certificates from Microsoft Entra ID (Azure AD) for use with the
// AADSSHLoginForLinux extension on Azure Arc-connected machines.
//
// The flow mirrors the Azure CLI "ssh" extension: an ephemeral RSA key pair is
// generated, its public key is presented to Entra ID as a proof-of-possession
// request (token_type=ssh-cert), and the returned token IS the OpenSSH
// certificate body. The certificate carries the login principal(s), so the SSH
// username is derived from the certificate rather than the token.
package aadssh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// rsaKeyBits is the size of the ephemeral RSA key pair. The Azure CLI ssh
// extension uses 2048-bit RSA keys.
const rsaKeyBits = 2048

// keyMaterial holds the values derived from the ephemeral key pair that are
// needed to request an SSH certificate from Entra ID.
type keyMaterial struct {
	publicKey ssh.PublicKey
	modulus   string // urlsafe base64 (with padding) of the SSH-wire modulus field
	exponent  string // urlsafe base64 (with padding) of the SSH-wire exponent field
	keyID     string // hex sha256(modulus || exponent)
	reqCnf    string // JSON JWK sent as the req_cnf token request parameter
}

// loadOrGenerateKey loads an existing RSA private key in OpenSSH, PKCS#1, or
// PKCS#8 format from path, or generates a new one and writes it (plus a
// "<path>.pub") only if the file does not yet exist. Any other read/parse error
// is returned rather than silently overwriting the existing file. Derived
// certificate-request material is computed from whichever key is used.
func loadOrGenerateKey(path string) (*keyMaterial, error) {
	priv, err := readPrivateKey(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read existing private key %s (refusing to overwrite): %w", path, err)
		}
		priv, err = rsa.GenerateKey(rand.Reader, rsaKeyBits)
		if err != nil {
			return nil, fmt.Errorf("generate rsa key: %w", err)
		}
		if err := writePrivateKey(path, priv); err != nil {
			return nil, err
		}
		if err := writePublicKey(path+".pub", priv); err != nil {
			return nil, err
		}
	}
	return deriveKeyMaterial(priv)
}

// deriveKeyMaterial computes the modulus/exponent/key_id/req_cnf values from a
// private key, matching the encoding used by the Azure CLI ssh extension.
func deriveKeyMaterial(priv *rsa.PrivateKey) (*keyMaterial, error) {
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("build ssh public key: %w", err)
	}

	// The SSH wire encoding of an ssh-rsa key is a sequence of length-prefixed
	// fields: [ "ssh-rsa", e, n ]. We base64url-encode the raw e and n bytes
	// (including any leading zero sign byte) exactly as the Azure CLI does.
	fields, err := sshWireFields(pub.Marshal())
	if err != nil {
		return nil, err
	}
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected ssh-rsa wire format: %d fields", len(fields))
	}
	exponent := base64.URLEncoding.EncodeToString(fields[1])
	modulus := base64.URLEncoding.EncodeToString(fields[2])

	h := sha256.New()
	h.Write([]byte(modulus))
	h.Write([]byte(exponent))
	keyID := hex.EncodeToString(h.Sum(nil))

	reqCnf, err := json.Marshal(struct {
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
		Kid string `json:"kid"`
	}{Kty: "RSA", N: modulus, E: exponent, Kid: keyID})
	if err != nil {
		return nil, fmt.Errorf("marshal req_cnf: %w", err)
	}

	return &keyMaterial{
		publicKey: pub,
		modulus:   modulus,
		exponent:  exponent,
		keyID:     keyID,
		reqCnf:    string(reqCnf),
	}, nil
}

// sshWireFields splits an SSH wire blob into its length-prefixed fields.
func sshWireFields(blob []byte) ([][]byte, error) {
	var fields [][]byte
	for len(blob) > 0 {
		if len(blob) < 4 {
			return nil, fmt.Errorf("truncated ssh wire field header")
		}
		n := binary.BigEndian.Uint32(blob[:4])
		blob = blob[4:]
		if uint32(len(blob)) < n {
			return nil, fmt.Errorf("truncated ssh wire field body")
		}
		fields = append(fields, blob[:n])
		blob = blob[n:]
	}
	return fields, nil
}

// readPrivateKey reads an RSA private key in a format supported by OpenSSH.
func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	priv, ok := raw.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key %s has type %T; RSA is required", path, raw)
	}
	return priv, nil
}

// writePrivateKey writes priv as a PEM PKCS#1 file with 0600 permissions,
// creating parent directories as needed. The write is atomic.
func writePrivateKey(path string, priv *rsa.PrivateKey) error {
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}
	if err := writeFileAtomic(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	return nil
}

// writePublicKey writes the authorized_keys-style public key ("ssh-rsa <b64>").
func writePublicKey(path string, priv *rsa.PrivateKey) error {
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, ssh.MarshalAuthorizedKey(pub), 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}
