package mpesa

// TEMPORARY (agent 4): compile-only stand-in for the real Daraja client that
// another agent writes in client.go. The integrator deletes this file at
// merge.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
)

// ClientOptions configure the Daraja client.
type ClientOptions struct {
	Env, BaseURL, ShortCode                             string
	ConsumerKey, ConsumerSecret, Passkey, CallbackToken config.Secret
	CallbackBaseURL                                     string
	Timeout                                             time.Duration
	Logger                                              *slog.Logger
	HTTPClient                                          *http.Client
	Now                                                 func() time.Time
}

// HTTPClient is the Daraja client.
type HTTPClient struct{ o ClientOptions }

var errStub = errors.New("mpesa: client stub (replaced at merge)")

// NewClient builds the client.
func NewClient(o ClientOptions) (*HTTPClient, error) { return &HTTPClient{o: o}, nil }

// CallbackURLs returns the callback URLs (they contain the secret token).
func (c *HTTPClient) CallbackURLs() (stk, c2bConfirmation, c2bValidation string) {
	base := c.o.CallbackBaseURL + "cb/mpesa/" + c.o.CallbackToken.Reveal()
	return base + "/stk", base + "/c2b/confirmation", base + "/c2b/validation"
}

// STKPush implements Client.
func (c *HTTPClient) STKPush(context.Context, STKPushRequest) (STKPushResponse, error) {
	return STKPushResponse{}, errStub
}

// STKQuery implements Client.
func (c *HTTPClient) STKQuery(context.Context, string) (STKQueryResult, error) {
	return STKQueryResult{}, errStub
}

// RegisterC2BURLs implements Client.
func (c *HTTPClient) RegisterC2BURLs(context.Context, RegisterURLsRequest) error { return errStub }
