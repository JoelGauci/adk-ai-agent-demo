package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Configuration variables for OAuth and MCP endpoints.
// In a production environment, these should be managed securely.
var (
	ClientID      string
	ClientSecret  string
	AuthURL       string
	TokenURL      string
	RedirectURI   string
	LegoAudience  = "mcp-lego"
	RobotAudience = "mcp-robot"
	LegoEndpoint  string
	RobotEndpoint string
)

// emailRegistry anchors verifiable user profiles to unguessable server-issued base tokens.
// This prevents client-side session spoofing or hijacking.
var emailRegistry = struct {
	sync.RWMutex
	mapping map[string]string
}{
	mapping: make(map[string]string),
}

// StoreEmailForToken binds a user's email to a specific token.
func StoreEmailForToken(token, email string) {
	emailRegistry.Lock()
	defer emailRegistry.Unlock()
	emailRegistry.mapping[token] = email
}

// GetEmailForToken retrieves the email associated with a token.
func GetEmailForToken(token string) (string, bool) {
	emailRegistry.RLock()
	defer emailRegistry.RUnlock()
	email, ok := emailRegistry.mapping[token]
	return email, ok
}

// Custom HTTP client with reasonable default timeout for auth calls.
var authHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
}

// codeStore preserves state and verifier across redirect.
var codeStore = struct {
	sync.Mutex
	states map[string]string
}{
	states: make(map[string]string),
}

func generateSecureString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generatePKCE() (verifier, challenge string, err error) {
	verifier, err = generateSecureString()
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])
	return verifier, challenge, nil
}

// BuildAuthURL constructs the OAuth2 authorization URL with PKCE challenge.
func BuildAuthURL() (string, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", err
	}

	state, err := generateSecureString()
	if err != nil {
		return "", err
	}

	codeStore.Lock()
	codeStore.states[state] = verifier
	codeStore.Unlock()

	u, err := url.Parse(AuthURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", "openid lego:read robot:read robot:write")
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", challenge)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode exchanges the inbound authorization code for an opaque Base Token along with profile details.
// This interacts with Apigee's identity facade.
func ExchangeCode(code, state string) (string, string, int, error) {
	codeStore.Lock()
	verifier, ok := codeStore.states[state]
	delete(codeStore.states, state)
	codeStore.Unlock()

	if !ok {
		return "", "", 0, fmt.Errorf("invalid or expired auth state")
	}

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", ClientID)
	data.Set("client_secret", ClientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", RedirectURI)
	data.Set("code_verifier", verifier)
	data.Set("scope", "openid lego:read robot:read robot:write")

	req, err := http.NewRequest("POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", "", 0, fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(b))
	}

	var res struct {
		AccessToken    string `json:"access_token"`
		DeveloperEmail string `json:"developer.email"`
		ExpiresIn      int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", "", 0, fmt.Errorf("failed decoding base token body: %v", err)
	}

	if res.AccessToken == "" {
		return "", "", 0, fmt.Errorf("server returned empty access token")
	}

	return res.AccessToken, res.DeveloperEmail, res.ExpiresIn, nil
}

// -- Token Exchange Cache Logic --

type tokenCacheItem struct {
	token  string
	expiry time.Time
}

var mcpTokenCache = struct {
	sync.RWMutex
	cache map[string]tokenCacheItem // keyed by sha256(baseToken):audience
}{
	cache: make(map[string]tokenCacheItem),
}

func getCacheKey(baseToken, audience string) string {
	h := sha256.Sum256([]byte(baseToken))
	return fmt.Sprintf("%x:%s", h, audience)
}

// GetTokenForAudience handles the Token Exchange routine with transparent logging/caching.
// This interacts with Apigee's identity facade for token exchange.
func GetTokenForAudience(ctx context.Context, baseToken string, audience string, logger func(t string, m string)) (string, error) {
	cacheKey := getCacheKey(baseToken, audience)

	mcpTokenCache.RLock()
	item, exists := mcpTokenCache.cache[cacheKey]
	mcpTokenCache.RUnlock()

	// Check for valid cache
	if exists && time.Now().Add(30*time.Second).Before(item.expiry) {
		return item.token, nil
	}

	if logger != nil {
		logger("EXCHANGE", fmt.Sprintf("Acquiring fresh exchange token for audience: %s", audience))
	}

	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("client_id", ClientID)
	data.Set("client_secret", ClientSecret)
	data.Set("subject_token", baseToken)
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("audience", audience)

	// CRITICAL: Dynamic Scoping to prevent security rejection due to cross-audience leakage
	reqScope := "openid"
	switch audience {
	case LegoAudience:
		reqScope = "openid lego:read"
	case RobotAudience:
		reqScope = "openid robot:read robot:write"
	}
	data.Set("scope", reqScope)

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if logger != nil {
		// Anonymize client_secret and subject_token for log safety
		loggedData := url.Values{}
		for k, v := range data {
			if k == "client_secret" || k == "subject_token" {
				loggedData[k] = []string{"[REDACTED]"}
			} else {
				loggedData[k] = v
			}
		}
		logger("URL", fmt.Sprintf("POST %s", TokenURL))
		logger("EXCHANGE", fmt.Sprintf("Token Exchange Request:\n%s", loggedData.Encode()))
	}

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Read body bytes first to allow both logging and parsing
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		if logger != nil {
			logger("ERROR", fmt.Sprintf("Exchange failed (%d): %s", resp.StatusCode, string(bodyBytes)))
		}
		return "", fmt.Errorf("exchange endpoint returned error code: %d", resp.StatusCode)
	}

	// Log the raw JSON response!
	if logger != nil {
		logger("TOKEN", fmt.Sprintf("Acquired dynamic token response for audience %s:\n%s", audience, string(bodyBytes)))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(strings.NewReader(string(bodyBytes))).Decode(&res); err != nil {
		return "", fmt.Errorf("failed to decode exchange response: %v", err)
	}

	if res.AccessToken == "" {
		return "", fmt.Errorf("server provided empty access token for audience %s", audience)
	}

	// Calculate expiry duration based on server, fallback to 5 minutes
	expiryDuration := 5 * time.Minute
	if res.ExpiresIn > 0 {
		expiryDuration = time.Duration(res.ExpiresIn) * time.Second
	}
	expiry := time.Now().Add(expiryDuration)

	if logger != nil {
		logger("TOKEN", fmt.Sprintf("Stored fresh dynamic token for audience %s. (Len:%d) Token: %s", audience, len(res.AccessToken), res.AccessToken))
	}

	mcpTokenCache.Lock()
	mcpTokenCache.cache[cacheKey] = tokenCacheItem{
		token:  res.AccessToken,
		expiry: expiry,
	}
	mcpTokenCache.Unlock()

	return res.AccessToken, nil
}

// TransportLoggerKey is the key for the logger in the context.
type TransportLoggerKey struct{}

// LogFunc allows dynamic callback injection across logical execution zones.
type LogFunc func(msgType string, message string)

// McpAuthTransport is a custom HTTP RoundTripper that injects bearer tokens.
type McpAuthTransport struct {
	Base      http.RoundTripper
	Audience  string
	BaseToken string
}

func (t *McpAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var logger LogFunc
	if l, ok := ctx.Value(TransportLoggerKey{}).(LogFunc); ok {
		logger = l
	}

	if logger != nil {
		logger("URL", fmt.Sprintf("%s %s", req.Method, req.URL.String()))
	}

	tok, err := GetTokenForAudience(ctx, t.BaseToken, t.Audience, logger)
	if err != nil {
		if logger != nil {
			logger("AUTH_REJECTED", "Halted request due to authorization failure.")
		}
		return nil, err
	}

	req2 := req.Clone(ctx)
	req2.Header.Set("Authorization", "Bearer "+tok)

	resp, err := t.Base.RoundTrip(req2)
	if err != nil {
		if logger != nil {
			logger("ERROR", fmt.Sprintf("Transport dispatch failed: %v", err))
		}
		return nil, err
	}

	if resp.StatusCode >= 400 && logger != nil {
		logger("ERROR", fmt.Sprintf("Target component responded with non-OK code: %d", resp.StatusCode))
	}

	return resp, nil
}
