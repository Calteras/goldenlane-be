// Command healthcheck exits 0 when the app in this container answers GET /healthz with
// 200 OK. It is the image's HEALTHCHECK: the production image has no shell or curl.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", http.NoBody)
	if err != nil {
		os.Exit(1)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Exit(1)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
