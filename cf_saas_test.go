package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newSaaSTestServer(t *testing.T, existing bool, existingOrigin string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			result := "[]"
			if existing {
				result = fmt.Sprintf(`[{"id":"abc123","custom_origin_server":%q,"status":"active"}]`, existingOrigin)
			}
			fmt.Fprintf(w, `{"success":true,"result":%s}`, result)
		case http.MethodPost:
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["hostname"] != "newsite.cowsaidmoo.tech" {
				t.Errorf("POST hostname = %v", body["hostname"])
			}
			if body["custom_origin_server"] != "pve2-origin.hosto.app" {
				t.Errorf("POST custom_origin_server = %v", body["custom_origin_server"])
			}
			fmt.Fprint(w, `{"success":true,"result":{"status":"pending"}}`)
		case http.MethodPatch:
			if r.URL.Path != "/zones/zone1/custom_hostnames/abc123" {
				t.Errorf("PATCH path = %s", r.URL.Path)
			}
			fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	return srv, &calls
}

func saasTestConfig(url string) Config {
	return Config{
		CFSaaSAPIToken:     "tok",
		CFSaaSZoneID:       "zone1",
		CFSaaSOriginServer: "pve2-origin.hosto.app",
	}
}

func TestEnsureCustomHostnameCreatesWhenMissing(t *testing.T) {
	srv, calls := newSaaSTestServer(t, false, "")
	defer srv.Close()
	orig := cfSaaSAPIBase
	cfSaaSAPIBase = srv.URL
	defer func() { cfSaaSAPIBase = orig }()

	status, err := ensureCustomHostname(saasTestConfig(srv.URL), "newsite.cowsaidmoo.tech")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if len(*calls) != 2 || (*calls)[1] != "POST /zones/zone1/custom_hostnames" {
		t.Errorf("calls = %v", *calls)
	}
}

func TestEnsureCustomHostnamePatchesWrongOrigin(t *testing.T) {
	srv, calls := newSaaSTestServer(t, true, "origin.hosto.app")
	defer srv.Close()
	orig := cfSaaSAPIBase
	cfSaaSAPIBase = srv.URL
	defer func() { cfSaaSAPIBase = orig }()

	status, err := ensureCustomHostname(saasTestConfig(srv.URL), "newsite.cowsaidmoo.tech")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if len(*calls) != 2 || (*calls)[1] != "PATCH /zones/zone1/custom_hostnames/abc123" {
		t.Errorf("calls = %v", *calls)
	}
}

func TestEnsureCustomHostnameNoopWhenAlreadyCorrect(t *testing.T) {
	srv, calls := newSaaSTestServer(t, true, "pve2-origin.hosto.app")
	defer srv.Close()
	orig := cfSaaSAPIBase
	cfSaaSAPIBase = srv.URL
	defer func() { cfSaaSAPIBase = orig }()

	status, err := ensureCustomHostname(saasTestConfig(srv.URL), "newsite.cowsaidmoo.tech")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if len(*calls) != 1 {
		t.Errorf("expected lookup only, calls = %v", *calls)
	}
}

func TestRegisterSaaSHostnameSkipsWhenUnconfigured(t *testing.T) {
	// Must not panic or make any network call.
	registerSaaSHostname(Config{}, "x.cowsaidmoo.tech")
}
