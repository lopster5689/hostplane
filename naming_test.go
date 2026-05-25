package main

import "testing"

// Naming functions are the single point of truth for resource names; if any
// of these golden mappings change, integration tests and operational scripts
// (health.sh, cleanup.sh) will break. Pin every one.

func TestNaming(t *testing.T) {
	cases := []struct {
		name     string
		got, want string
	}{
		{"PHPContainerName", PHPContainerName("mysite"), "php_mysite"},
		{"PHPContainerName/digits", PHPContainerName("smoke1715784000"), "php_smoke1715784000"},
		{"VolumeName", VolumeName("mysite"), "wp_mysite"},
		{"WPDatabaseName", WPDatabaseName("mysite"), "wp_mysite"},
		{"WPDatabaseUser", WPDatabaseUser("mysite"), "wp_mysite"},
		{"WPDatabasePass", WPDatabasePass("mysite"), "***REMOVED***"},
		{"SiteDomain", SiteDomain("mysite", "cowsaidmoo.tech"), "mysite.cowsaidmoo.tech"},
		{"SiteDomain/single-label-base", SiteDomain("a", "b"), "a.b"},
		{"CaddyConfFile", CaddyConfFile("mysite"), "mysite.caddy"},
		{"NginxContainerName", NginxContainerName("mysite"), "nginx_mysite"},
		{"NginxConfFile", NginxConfFile("mysite"), "mysite.conf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}
