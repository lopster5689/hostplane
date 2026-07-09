package main

import (
	"strings"
	"testing"
)

// TestVerifyPageChecks locks out the regression that shipped a broken store as
// "verified": a default-blog homepage and a bare 301 must FAIL, a real store must PASS.
func TestVerifyPageChecks(t *testing.T) {
	pad := strings.Repeat("x", 300)
	realStore := `<!doctype html><html><head><title>Crumb & Co</title>` +
		`<script src="https://cdn.tailwindcss.com"></script></head><body>` +
		`<h1>Crumb & Co</h1><div class="product">Golden Croissant <span class="price">120</span></div>` +
		`<div class="product">Artisan Sourdough</div>` + pad + `</body></html>`
	// The EXACT failure that shipped: the front page fell back to the stock blog.
	defaultBlog := `<!doctype html><html><head><title>Meri Dukaan</title></head><body>` +
		`<h2>Hello world!</h2><p>Welcome to WordPress. This is your first post. ` +
		`Edit or delete it, then start writing!</p>` + pad + `</body></html>`
	phpError := `<br /><b>Fatal error</b>: Uncaught Error in /var/www/html/functions.php on line 42` + pad

	cases := []struct {
		name           string
		body, status   string
		expect, absent []string
		wantOK         bool
		wantToken      string
	}{
		{"real store passes", realStore, "200", nil, nil, true, ""},
		{"real store with product asserts passes", realStore, "200",
			[]string{"Golden Croissant", "120"}, []string{"Hello world!"}, true, ""},
		{"DEFAULT BLOG must FAIL (the shipped bug)", defaultBlog, "200", nil, nil, false, "default_wp_content"},
		{"bare 301 must FAIL (the /shop bug)", "", "301", nil, nil, false, "final_status=301"},
		{"php fatal must FAIL", phpError, "200", nil, nil, false, "php_fatal_error"},
		{"missing expected product FAILs", realStore, "200", []string{"Chocolate Cake"}, nil, false, "missing:Chocolate Cake"},
		{"forbidden content present FAILs", defaultBlog, "200", nil, []string{"Hello world!"}, false, "present_but_forbidden"},
		{"empty body FAILs", "", "200", nil, nil, false, "body_too_small"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fails := verifyPageChecks(c.body, c.status, c.expect, c.absent)
			if ok := len(fails) == 0; ok != c.wantOK {
				t.Fatalf("verdict: got ok=%v (fails=%v), want ok=%v", ok, fails, c.wantOK)
			}
			if c.wantToken != "" {
				found := false
				for _, f := range fails {
					if strings.Contains(f, c.wantToken) {
						found = true
					}
				}
				if !found {
					t.Fatalf("want a fail token containing %q, got %v", c.wantToken, fails)
				}
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b ,,c ")
	want := []string{"a", "b", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	if splitList("") != nil {
		t.Fatalf("empty should be nil")
	}
}

func TestVerifyPageCatchesEscapedHTML(t *testing.T) {
	body := "<html><title>Contact</title><body>&lt;h2&gt;Visit Us&lt;/h2&gt; and more text making this long enough to pass the size floor. " +
		"padding padding padding padding padding padding padding padding padding padding</body></html>"
	fails := verifyPageChecks(body, "200", nil, nil)
	found := false
	for _, f := range fails {
		if strings.Contains(f, "escaped_html_visible") {
			found = true
		}
	}
	if !found {
		t.Errorf("escaped-markup page must fail, got %v", fails)
	}
	clean := "<html><title>Contact</title><body><h2>Visit Us</h2> real markup, plenty of text here to pass the minimum body size check for the verifier padding padding padding padding padding</body></html>"
	for _, f := range verifyPageChecks(clean, "200", nil, nil) {
		if strings.Contains(f, "escaped_html_visible") {
			t.Errorf("clean page wrongly flagged: %v", f)
		}
	}
}
