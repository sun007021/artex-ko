package siwc

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Identity is the verified account identity from an ID token. Subject is the
// account identity; email and sub are NOT workspace identifiers, which is why the
// issued client id is stored alongside it.
type Identity struct {
	Subject string
	Email   string
	Name    string
}

type idClaims struct {
	jwt.RegisteredClaims
	Nonce string `json:"nonce"`
	Azp   string `json:"azp"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

const jwksTTL = time.Hour

// VerifyIDToken verifies an ID token: RS256 signature against the issuer's
// published JWKS, issuer, audience == the issued client id, expiry, and (when
// wantNonce != "") the nonce saved for this attempt. validAt is the time at which
// expiry is evaluated: pass the response receipt time so an outage that outlasts
// the token's lifetime does not invalidate an identity that was fine when received.
//
// A JWKS that cannot be fetched yields ErrIdentityUnavailable (retryable; the
// connection must be preserved), a token that fails verification yields
// invalid_id_token.
func (c *Client) VerifyIDToken(ctx context.Context, idToken, clientID, wantNonce string, validAt time.Time) (*Identity, error) {
	if validAt.IsZero() {
		validAt = c.now()
	}
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return c.publicKey(ctx, kid)
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(c.issuer),
		jwt.WithAudience(clientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
		jwt.WithTimeFunc(func() time.Time { return validAt }),
	)
	var claims idClaims
	tok, err := parser.ParseWithClaims(idToken, &claims, keyfunc)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) && ae.Code == CodeIdentityUnavailable {
			return nil, newErr(CodeIdentityUnavailable, true)
		}
		return nil, newErr(CodeInvalidIDToken, false)
	}
	if !tok.Valid || claims.Subject == "" || claims.IssuedAt == nil ||
		(wantNonce != "" && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(wantNonce)) != 1) ||
		(claims.Azp != "" && claims.Azp != clientID) ||
		(len(claims.Audience) > 1 && claims.Azp != clientID) {
		return nil, newErr(CodeInvalidIDToken, false)
	}
	return &Identity{Subject: claims.Subject, Email: claims.Email, Name: claims.Name}, nil
}

// publicKey returns the RSA key for kid, refetching the JWKS once when the kid is
// unknown (key rotation) or the cache is stale.
func (c *Client) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	lookup := func() *rsa.PublicKey {
		if k, ok := c.keys[kid].(*rsa.PublicKey); ok {
			return k
		}
		if kid == "" && len(c.keys) == 1 { // single-key set, token without kid
			for _, v := range c.keys {
				k, _ := v.(*rsa.PublicKey)
				return k
			}
		}
		return nil
	}
	if c.keys != nil && c.now().Sub(c.keysAt) < jwksTTL {
		if k := lookup(); k != nil {
			return k, nil
		}
	}
	if err := c.fetchJWKS(ctx); err != nil {
		return nil, err
	}
	if k := lookup(); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("signing key not found") // verification failure, not unavailability
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (c *Client) fetchJWKS(ctx context.Context) error {
	d, err := c.getDiscovery(ctx)
	if err != nil {
		return newErr(CodeIdentityUnavailable, true)
	}
	resp, body, err := c.do(ctx, http.MethodGet, d.JWKSURI, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return newErr(CodeIdentityUnavailable, true)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if json.Unmarshal(body, &set) != nil || len(set.Keys) == 0 {
		return newErr(CodeIdentityUnavailable, true)
	}
	keys := map[string]any{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			continue
		}
		ev := 0
		for _, b := range e {
			ev = ev<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ev}
	}
	if len(keys) == 0 {
		return newErr(CodeIdentityUnavailable, true)
	}
	c.keys, c.keysAt = keys, c.now()
	return nil
}
