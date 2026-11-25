package mpesa

import "context"

// CheckCredentials asks Daraja for a fresh OAuth access token with this
// client's consumer key and secret, bypassing the token cache. A nil error
// means Safaricom accepted the credentials. The token is cached for later
// calls and never returned. (The passkey cannot be checked this way: only
// an STK push would show it is wrong.)
func (c *HTTPClient) CheckCredentials(ctx context.Context) error {
	if _, err := c.fetchToken(ctx); err != nil {
		return err
	}
	return nil
}

// Env is this client's Daraja environment (sandbox or production).
func (c *HTTPClient) Env() string { return c.env }

// ShortCode is the BusinessShortCode this client uses for STK pushes.
func (c *HTTPClient) ShortCode() string { return c.shortCode }
