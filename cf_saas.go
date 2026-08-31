package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// cfSaaSAPIBase is a var so tests can point it at an httptest server.
var cfSaaSAPIBase = "https://api.cloudflare.com/client/v4"

// registerSaaSHostname makes a freshly provisioned <site>.<BaseDomain> publicly
// reachable by registering it as a Cloudflare-for-SaaS custom hostname on the
// hosto.app zone. Without this, the base-domain wildcard (a proxied CNAME to
// customers.hosto.app in a founder-held CF account) hands every unregistered
// slug a 403/1014 at the edge — the failure mode that kept cowsaidmoo slugs
// dark after the 2026-08 VPS loss.
//
// Non-fatal by design: the site itself is provisioned and serving at the
// origin; a registration failure only delays public reachability and is
// recoverable manually via hosto-infra/scripts/cowsaidmoo_slug.sh, so it must
// never roll back a completed provision. Skipped (with a log line) when the
// CF_SAAS_* env is not configured.
//
// Idempotent: an existing hostname is PATCHed (origin override) rather than
// re-created, which also sidesteps CF's "deleted names stay reserved" 1406
// behaviour on re-provisioned slugs.
func registerSaaSHostname(cfg Config, domain string) {
	if cfg.CFSaaSAPIToken == "" || cfg.CFSaaSZoneID == "" {
		log.Printf("[cf-saas] skipped for %s: CF_SAAS_API_TOKEN/CF_SAAS_ZONE_ID not configured", domain)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		status, err := ensureCustomHostname(cfg, domain)
		if err == nil {
			log.Printf("[cf-saas] site domain %s registered (status=%s, origin=%s)", domain, status, cfg.CFSaaSOriginServer)
			return
		}
		lastErr = err
	}
	log.Printf("[cf-saas] WARNING: %s NOT registered after 3 attempts (%v) — site serves at origin but the edge returns 1014 until `cowsaidmoo_slug.sh add` is run", domain, lastErr)
}

// ensureCustomHostname looks the hostname up and PATCHes or POSTs accordingly.
// Returns the CF hostname status (e.g. "pending", "active").
func ensureCustomHostname(cfg Config, domain string) (string, error) {
	id, origin, status, err := lookupCustomHostname(cfg, domain)
	if err != nil {
		return "", err
	}

	if id != "" && origin == cfg.CFSaaSOriginServer {
		return status, nil // already registered as desired
	}

	body := map[string]interface{}{
		"ssl": map[string]string{"method": "http", "type": "dv"},
	}
	if cfg.CFSaaSOriginServer != "" {
		body["custom_origin_server"] = cfg.CFSaaSOriginServer
	}

	method, url := http.MethodPost, fmt.Sprintf("%s/zones/%s/custom_hostnames", cfSaaSAPIBase, cfg.CFSaaSZoneID)
	if id != "" {
		method, url = http.MethodPatch, url+"/"+id
	} else {
		body["hostname"] = domain
	}

	respBody, err := cfSaaSRequest(cfg, method, url, body)
	if err != nil {
		return "", err
	}
	var resp struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("decode %s response: %w", method, err)
	}
	if !resp.Success {
		return "", fmt.Errorf("%s custom hostname failed: %+v", method, resp.Errors)
	}
	return resp.Result.Status, nil
}

func lookupCustomHostname(cfg Config, domain string) (id, origin, status string, err error) {
	url := fmt.Sprintf("%s/zones/%s/custom_hostnames?hostname=%s", cfSaaSAPIBase, cfg.CFSaaSZoneID, domain)
	respBody, err := cfSaaSRequest(cfg, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID                 string `json:"id"`
			CustomOriginServer string `json:"custom_origin_server"`
			Status             string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", "", "", fmt.Errorf("decode lookup response: %w", err)
	}
	if !resp.Success {
		return "", "", "", fmt.Errorf("custom hostname lookup failed for %s", domain)
	}
	if len(resp.Result) == 0 {
		return "", "", "", nil
	}
	return resp.Result[0].ID, resp.Result[0].CustomOriginServer, resp.Result[0].Status, nil
}

func cfSaaSRequest(cfg Config, method, url string, body interface{}) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CFSaaSAPIToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
