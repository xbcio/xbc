package oauth2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// fetchUserInfo performs the one userinfo call of a login and returns the
// decoded JSON object. The access token travels only in the Authorization
// header, the request goes to the registered endpoint verbatim, and the
// provider's userinfo client rejects every redirect, so a response cannot be
// served from a location the application did not register.
func (p *Provider) fetchUserInfo(ctx context.Context, accessToken string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userInfoEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")

	response, err := p.userInfoClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: userinfo endpoint returned status %d", ErrUserInfo, response.StatusCode)
	}
	body, err := outbound.ReadBody(response, p.policy.MaxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
	}

	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: response is not a JSON object", ErrInvalidUserInfo)
	}
	if document == nil {
		return nil, fmt.Errorf("%w: response is not a JSON object", ErrInvalidUserInfo)
	}
	return document, nil
}

// fieldAt resolves a dotted path against nested JSON objects.
func fieldAt(document map[string]any, path []string) (any, bool) {
	var current any = document
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
