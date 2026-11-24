package fcm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/fcm"
)

func client(t *testing.T, status int, body string) (*fcm.Client, *http.Request, *[]byte) {
	t.Helper()

	var (
		seen http.Request
		raw  []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := fcm.NewWithTokenSource(srv.URL, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "access-123"}))

	return c, &seen, &raw
}

func TestSend(t *testing.T) {
	c, seen, raw := client(t, 200, `{"name":"projects/p/messages/1"}`)

	err := c.Send(context.Background(), "device-token", "Pesanan siap", "Ambil di Kios", map[string]string{"orderId": "o1", "status": "READY"})
	require.NoError(t, err)
	require.Equal(t, "Bearer access-123", seen.Header.Get("Authorization"))

	var body struct {
		Message struct {
			Token        string            `json:"token"`
			Notification map[string]string `json:"notification"`
			Data         map[string]string `json:"data"`
			Android      map[string]any    `json:"android"`
		} `json:"message"`
	}

	require.NoError(t, json.Unmarshal(*raw, &body))
	require.Equal(t, "device-token", body.Message.Token)
	require.Equal(t, "Pesanan siap", body.Message.Notification["title"])
	require.Equal(t, "o1", body.Message.Data["orderId"])
	require.Equal(t, "HIGH", body.Message.Android["priority"], "delivered promptly, not batched")
}

func TestSendErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		body         string
		unregistered bool
	}{
		"token gone":      {404, `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`, true},
		"token malformed": {400, `{"error":{"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`, true},
		"other bad input": {400, `{"error":{"status":"INVALID_ARGUMENT","message":"Invalid JSON payload"}}`, false},
		"throttled":       {429, `{}`, false},
		"provider down":   {503, `{}`, false},
		"bad credentials": {401, `{}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, _, _ := client(t, tc.status, tc.body)

			err := c.Send(context.Background(), "t", "a", "b", nil)
			require.Error(t, err)
			require.Equal(t, tc.unregistered, err == entity.ErrUnregistered)
		})
	}
}

func TestNewRejectsAMissingOrBrokenKeyFile(t *testing.T) {
	_, err := fcm.New(context.Background(), "/nonexistent.json", "p")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "nonexistent", "the path is not echoed")
}
