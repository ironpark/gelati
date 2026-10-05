package codex

import (
	"context"
	"encoding/json/jsontext"
	"fmt"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Account type discriminators returned by account/read.
const (
	AccountAPIKey        = "apiKey"
	AccountChatGPT       = "chatgpt"
	AccountAmazonBedrock = "amazonBedrock"
)

// Auth modes reported by account/updated.
const (
	AuthModeAPIKey              = "apikey"
	AuthModeChatGPT             = "chatgpt"
	AuthModeChatGPTAuthTokens   = "chatgptAuthTokens"
	AuthModeHeaders             = "headers"
	AuthModeBedrockAccessKeys   = "bedrockAccessKeys"
	AuthModeAgentIdentity       = "agentIdentity"
	AuthModePersonalAccessToken = "personalAccessToken"
	AuthModeBedrockAPIKey       = "bedrockApiKey"
)

// Account describes the signed-in account.
type Account struct {
	// Type is one of the Account* constants.
	Type string `json:"type"`
	// Email is the ChatGPT account email; it is nil when unavailable.
	Email *string `json:"email,omitzero"`
	// PlanType is the ChatGPT plan, such as "pro" or "plus".
	PlanType string `json:"planType,omitempty"`
	// UsesCodexManagedCredentials is set for Amazon Bedrock accounts whose
	// credentials Codex manages.
	UsesCodexManagedCredentials bool `json:"usesCodexManagedCredentials,omitzero"`
}

// AccountInfo is the account/read response.
type AccountInfo struct {
	// Account is nil when no account is signed in.
	Account *Account `json:"account"`
	// RequiresOpenaiAuth reports whether the active provider needs OpenAI
	// credentials.
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

// ChatGPTLogin is the result of starting the browser login flow.
type ChatGPTLogin struct {
	Type    string `json:"type"`
	LoginID string `json:"loginId"`
	// AuthURL must be opened in a browser; the app-server hosts the callback.
	AuthURL string `json:"authUrl"`
}

// DeviceCodeLogin is the result of starting the device-code login flow.
type DeviceCodeLogin struct {
	Type    string `json:"type"`
	LoginID string `json:"loginId"`
	// VerificationURL and UserCode are shown to the user.
	VerificationURL string `json:"verificationUrl"`
	UserCode        string `json:"userCode"`
}

// ChatGPTLoginOptions tunes the browser login flow.
type ChatGPTLoginOptions struct {
	// UseHostedLoginSuccessPage redirects to the hosted success page when
	// organization setup is not required.
	UseHostedLoginSuccessPage bool
	// AppBrand is "codex" or "chatgpt"; it applies only with the hosted
	// success page and defaults to "codex".
	AppBrand string
}

// LoginCompletedParams is the payload of account/login/completed. LoginID is
// empty for logins that have no id, such as API-key logins.
type LoginCompletedParams struct {
	LoginID string `json:"loginId,omitempty"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// AccountUpdate is the payload of account/updated. AuthMode is empty after
// logout.
type AccountUpdate struct {
	AuthMode string `json:"authMode,omitempty"`
	PlanType string `json:"planType,omitempty"`
}

// ReadAccount fetches the current account info. Set refreshToken to force a
// token refresh in managed ChatGPT mode.
func (c *Client) ReadAccount(ctx context.Context, refreshToken bool) (*AccountInfo, error) {
	params := struct {
		RefreshToken bool `json:"refreshToken"`
	}{RefreshToken: refreshToken}
	var info AccountInfo
	if err := c.call(ctx, "account/read", params, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// LoginAPIKey signs in with an OpenAI API key.
func (c *Client) LoginAPIKey(ctx context.Context, apiKey string) error {
	params := struct {
		Type   string `json:"type"`
		APIKey string `json:"apiKey"`
	}{Type: AccountAPIKey, APIKey: apiKey}
	return c.call(ctx, "account/login/start", params, nil)
}

// LoginChatGPT starts the managed ChatGPT browser flow. Await completion with
// AwaitLogin using the returned login id.
func (c *Client) LoginChatGPT(ctx context.Context, opts *ChatGPTLoginOptions) (*ChatGPTLogin, error) {
	params := struct {
		Type                      string `json:"type"`
		UseHostedLoginSuccessPage bool   `json:"useHostedLoginSuccessPage,omitzero"`
		AppBrand                  string `json:"appBrand,omitempty"`
	}{Type: AccountChatGPT}
	if opts != nil {
		params.UseHostedLoginSuccessPage = opts.UseHostedLoginSuccessPage
		params.AppBrand = opts.AppBrand
	}
	var login ChatGPTLogin
	if err := c.call(ctx, "account/login/start", params, &login); err != nil {
		return nil, err
	}
	return &login, nil
}

// LoginChatGPTDeviceCode starts the ChatGPT device-code flow.
func (c *Client) LoginChatGPTDeviceCode(ctx context.Context) (*DeviceCodeLogin, error) {
	params := struct {
		Type string `json:"type"`
	}{Type: "chatgptDeviceCode"}
	var login DeviceCodeLogin
	if err := c.call(ctx, "account/login/start", params, &login); err != nil {
		return nil, err
	}
	return &login, nil
}

// CancelLogin cancels a pending managed ChatGPT login.
func (c *Client) CancelLogin(ctx context.Context, loginID string) error {
	params := struct {
		LoginID string `json:"loginId"`
	}{LoginID: loginID}
	return c.call(ctx, "account/login/cancel", params, nil)
}

// Logout signs out of the current account.
func (c *Client) Logout(ctx context.Context) error {
	return c.call(ctx, "account/logout", nil, nil)
}

// AccountUpdates returns the client's account/updated channel. Updates are
// dropped rather than queued without bound, so read them promptly.
func (c *Client) AccountUpdates() <-chan AccountUpdate { return c.accounts }

// AwaitLogin waits for the account/login/completed notification matching
// loginID. Use the empty string for flows with no login id, such as API-key
// login. A completion for a non-empty loginID that arrived before the call is
// kept for it, so the wait may start after the login does; for the empty id,
// start waiting before the login.
func (c *Client) AwaitLogin(ctx context.Context, loginID string) (*LoginCompletedParams, error) {
	waiter := make(chan *LoginCompletedParams, 1)

	c.mu.Lock()
	if early, ok := c.loginResults[loginID]; ok {
		delete(c.loginResults, loginID)
		waiter <- early
	} else {
		c.logins[loginID] = append(c.logins[loginID], waiter)
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		waiters := c.logins[loginID]
		for index, candidate := range waiters {
			if candidate == waiter {
				c.logins[loginID] = append(waiters[:index], waiters[index+1:]...)
				break
			}
		}
		if len(c.logins[loginID]) == 0 {
			delete(c.logins, loginID)
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.tr.Done():
		return nil, ErrClosed
	case completed := <-waiter:
		return completed, completed.err()
	}
}

// err reports a failed login as an error.
func (p *LoginCompletedParams) err() error {
	if p.Success {
		return nil
	}
	message := p.Error
	if message == "" {
		message = "login failed"
	}
	return fmt.Errorf("codex: %s", message)
}

// maxEarlyLogins bounds the completions kept for logins nobody awaits yet.
const maxEarlyLogins = 16

// routeAccountNotification dispatches account notifications to waiters.
func (c *Client) routeAccountNotification(method string, params jsontext.Value) {
	switch method {
	case MethodLoginCompleted:
		var payload LoginCompletedParams
		if err := jsonx.Unmarshal(params, &payload); err != nil {
			c.logger.Debug("codex: bad login/completed payload", "error", err)
			return
		}
		c.mu.Lock()
		waiters := append([]chan *LoginCompletedParams(nil), c.logins[payload.LoginID]...)
		if len(waiters) == 0 && payload.LoginID != "" {
			// Keep it for an AwaitLogin that has not started yet. Logins
			// without an id are not kept: a later wait could not tell a
			// stale completion from its own.
			if len(c.loginResults) >= maxEarlyLogins {
				// Make room by dropping one; which one does not matter.
				for id := range c.loginResults {
					delete(c.loginResults, id)
					break
				}
			}
			c.loginResults[payload.LoginID] = &payload
		}
		c.mu.Unlock()
		for _, waiter := range waiters {
			select {
			case waiter <- &payload:
			default:
			}
		}
	case MethodAccountUpdated:
		var payload AccountUpdate
		if err := jsonx.Unmarshal(params, &payload); err != nil {
			c.logger.Debug("codex: bad account/updated payload", "error", err)
			return
		}
		select {
		case c.accounts <- payload:
		default:
			c.logger.Debug("codex: dropped account update", "authMode", payload.AuthMode)
		}
	}
}
