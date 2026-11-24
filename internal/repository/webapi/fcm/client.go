// Package fcm sends push notifications through Firebase Cloud Messaging (HTTP v1 API).
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const _scope = "https://www.googleapis.com/auth/firebase.messaging"

// Client sends messages as a service account.
type Client struct {
	http     *http.Client
	tokens   oauth2.TokenSource
	endpoint string
}

// New reads a Firebase service account key file. The credentials never leave this process
// except as short-lived access tokens.
func New(ctx context.Context, serviceAccountFile, projectID string) (*Client, error) {
	raw, err := os.ReadFile(serviceAccountFile) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, errors.New("fcm: cannot read the service account file")
	}

	conf, err := google.JWTConfigFromJSON(raw, _scope)
	if err != nil {
		return nil, errors.New("fcm: the service account file is not valid")
	}

	return &Client{
		http:     &http.Client{Timeout: 10 * time.Second},
		tokens:   conf.TokenSource(ctx),
		endpoint: "https://fcm.googleapis.com/v1/projects/" + projectID + "/messages:send",
	}, nil
}

// NewWithTokenSource builds a client against any endpoint (tests).
func NewWithTokenSource(endpoint string, tokens oauth2.TokenSource) *Client {
	return &Client{http: &http.Client{Timeout: 10 * time.Second}, tokens: tokens, endpoint: endpoint}
}

// Send pushes one message to one device. data values must be strings.
func (c *Client) Send(ctx context.Context, deviceToken, title, body string, data map[string]string) error {
	tok, err := c.tokens.Token()
	if err != nil {
		return fmt.Errorf("fcm: no access token: %w", err)
	}

	payload, err := json.Marshal(map[string]any{"message": map[string]any{
		"token":        deviceToken,
		"notification": map[string]string{"title": title, "body": body},
		"data":         data,
		"android":      map[string]any{"priority": "HIGH"},
		"apns":         map[string]any{"headers": map[string]string{"apns-priority": "10"}},
	}})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// A dead token is 404 UNREGISTERED, or 400 INVALID_ARGUMENT naming the token field.
	if resp.StatusCode == http.StatusNotFound || bytes.Contains(raw, []byte("UNREGISTERED")) ||
		(resp.StatusCode == http.StatusBadRequest && strings.Contains(string(raw), "registration token")) {
		return entity.ErrUnregistered
	}

	return fmt.Errorf("fcm: status %d", resp.StatusCode)
}
