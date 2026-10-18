package oauth2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// exchangeCode redeems an authorization code with PKCE at the token endpoint
// and returns the access token. The token is used for exactly one userinfo
// call and is never stored, logged, or returned to a caller.
//
// The request is a form POST: credentials travel in the Authorization header
// (client_secret_basic) or the request body (client_secret_post), never in the
// URL. A non-success status is reported by its status code alone: the IdP
// response body is never echoed into an error.
func (p *Provider) exchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.redirectURI)
	form.Set("code_verifier", verifier)
	useBasic := p.authMethod == ClientSecretBasic
	if !useBasic {
		form.Set("client_id", p.clientID)
		form.Set("client_secret", p.clientSecret)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTokenExchange, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if useBasic {
		// RFC 6749 section 2.3.1: the client identifier and secret are
		// form-urlencoded before they are joined and base64-encoded.
		request.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(p.clientSecret))
	}

	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTokenExchange, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint returned status %d", ErrTokenExchange, response.StatusCode)
	}
	body, err := outbound.ReadBody(response, p.policy.MaxBodyBytes)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTokenExchange, err)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("%w: response is not a JSON object", ErrInvalidTokenResponse)
	}
	if !strings.EqualFold(payload.TokenType, "Bearer") {
		return "", fmt.Errorf("%w: token_type is not Bearer", ErrInvalidTokenResponse)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("%w: response carries no access_token", ErrInvalidTokenResponse)
	}
	return payload.AccessToken, nil
}
