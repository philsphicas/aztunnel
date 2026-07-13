package aadssh

// sshCertScheme implements the MSAL Go authority.AuthenticationScheme
// extensibility interface (exposed as public.AuthenticationScheme) to request a
// token_type=ssh-cert proof-of-possession token from Entra ID. The returned
// "access token" is the OpenSSH certificate body itself.
type sshCertScheme struct {
	reqCnf string
	keyID  string
}

// TokenRequestParams returns the extra /token endpoint parameters that turn a
// normal token request into an SSH certificate request.
func (s *sshCertScheme) TokenRequestParams() map[string]string {
	return map[string]string{
		"token_type": "ssh-cert",
		"req_cnf":    s.reqCnf,
		"key_id":     s.keyID,
	}
}

// KeyID returns the identifier binding the issued certificate to our key pair.
func (s *sshCertScheme) KeyID() string { return s.keyID }

// FormatAccessToken returns the certificate body unchanged; unlike a bearer
// token it is not wrapped in an Authorization header by this tool.
func (s *sshCertScheme) FormatAccessToken(accessToken string) (string, error) {
	return accessToken, nil
}

// AccessTokenType matches the token_type returned by Entra ID so MSAL can
// distinguish cached ssh-cert tokens from bearer tokens.
func (s *sshCertScheme) AccessTokenType() string { return "ssh-cert" }
