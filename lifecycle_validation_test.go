package main

import (
	"strings"
	"testing"
)

func TestValidateDomainFormat(t *testing.T) {
	cases := []struct {
		name    string
		domain  string
		wantErr string // empty == expect nil
	}{
		{"empty", "", "empty"},
		{"too-long", strings.Repeat("a", 254) + ".com", "too long"},
		{"wildcard", "*.example.com", "wildcard"},
		{"single-label", "example", "invalid"},
		{"trailing-dot", "example.com.", "invalid"},
		{"invalid-char", "exa mple.com", "invalid"},
		{"hyphen-edge-start", "-example.com", "invalid"},
		{"hyphen-edge-end", "example-.com", "invalid"},
		{"all-numeric-tld", "example.123", "invalid"},
		{"valid-simple", "example.com", ""},
		{"valid-subdomain", "blog.example.com", ""},
		{"valid-deep", "a.b.c.example.com", ""},
		{"valid-hyphen", "my-site.example.com", ""},
		{"valid-numeric-prefix", "123.example.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDomainFormat(c.domain)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateDomainFormat(%q) = %v, want nil", c.domain, err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), c.wantErr) {
				t.Errorf("ValidateDomainFormat(%q) error = %v, want substring %q", c.domain, err, c.wantErr)
			}
		})
	}
}

func TestValidateDomainNotBase(t *testing.T) {
	base := "cowsaidmoo.tech"
	cases := []struct {
		name    string
		domain  string
		wantErr bool
	}{
		{"is-base", "cowsaidmoo.tech", true},
		{"is-subdomain-of-base", "anything.cowsaidmoo.tech", true},
		{"deep-subdomain-of-base", "a.b.cowsaidmoo.tech", true},
		{"unrelated", "example.com", false},
		{"superstring-not-suffix", "newcowsaidmoo.tech", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDomainNotBase(c.domain, base)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateDomainNotBase(%q,%q) err=%v wantErr=%v", c.domain, base, err, c.wantErr)
			}
		})
	}
}

func TestValidateCustomDomain(t *testing.T) {
	// Composite: must reject if format OR base-overlap fails. Spot-check both paths.
	base := "cowsaidmoo.tech"
	if err := ValidateCustomDomain("example.com", base); err != nil {
		t.Errorf("valid example.com rejected: %v", err)
	}
	if err := ValidateCustomDomain("a.cowsaidmoo.tech", base); err == nil {
		t.Error("subdomain of base must be rejected")
	}
	if err := ValidateCustomDomain("*.example.com", base); err == nil {
		t.Error("wildcard must be rejected")
	}
}

func TestCheckDomainDNS(t *testing.T) {
	// We can't make assertions about real DNS in unit tests, but we can verify
	// the result shape on a guaranteed-bogus input.
	r := CheckDomainDNS("definitely-does-not-exist-12345.invalid", "***REMOVED***")
	if r.PointsToIP {
		t.Error("PointsToIP must be false for unresolvable host")
	}
	if r.Resolved == nil {
		t.Error("Resolved must be a non-nil slice on lookup failure (got nil — caller may panic)")
	}
}
