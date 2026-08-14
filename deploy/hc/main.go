// Command hc is a minimal HTTP healthcheck for the kvp container. The
// Docker Hardened Image runtime has no shell or wget, so readiness is probed
// with this static binary instead (exec-form HEALTHCHECK).
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	url := os.Getenv("KVP_HEALTH_URL")
	if url == "" {
		url = "http://127.0.0.1:9090/readyz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %s\n", url, resp.Status)
		os.Exit(1)
	}
}
