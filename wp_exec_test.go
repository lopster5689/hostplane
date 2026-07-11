package main

import (
	"strings"
	"testing"
)

// buildTool is the security boundary: every call from the HTTP handler goes
// through this switch. Tests assert (a) the argv shape for each tool, and
// (b) every documented validation failure (slug regex, SSRF guard, option
// allowlist, etc.) actually rejects.

func TestBuildTool_HappyPaths(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		params map[string]string
		wantCmd []string // exact match
	}{
		{
			"get_site_info",
			"get_site_info", nil,
			[]string{"wp", "option", "get", "blogname", "--format=json", "--allow-root"},
		},
		{
			"get_option-blogname",
			"get_option", map[string]string{"key": "blogname"},
			[]string{"wp", "option", "get", "blogname", "--format=json", "--allow-root"},
		},
		{
			"update_option-blogname",
			"update_option", map[string]string{"key": "blogname", "value": "Hello"},
			[]string{"wp", "option", "update", "blogname", "Hello", "--allow-root"},
		},
		{
			"list_posts-default",
			"list_posts", nil,
			[]string{"wp", "post", "list", "--post_type=post", "--format=json", "--allow-root"},
		},
		{
			"list_posts-page-publish",
			"list_posts", map[string]string{"post_type": "page", "status": "publish"},
			[]string{"wp", "post", "list", "--post_type=page", "--format=json", "--allow-root", "--post_status=publish"},
		},
		{
			"get_post",
			"get_post", map[string]string{"id": "42"},
			[]string{"wp", "post", "get", "42", "--format=json", "--allow-root"},
		},
		{
			"update_post-title",
			"update_post", map[string]string{"id": "42", "field": "post_title", "value": "new"},
			[]string{"wp", "post", "update", "42", "--post_title=new", "--user=1", "--allow-root"},
		},
		{
			"create_post-default-page",
			"create_post", map[string]string{"post_title": "Hello"},
			[]string{"wp", "post", "create", "--post_type=page", "--post_title=Hello", "--post_status=draft", "--user=1", "--allow-root"},
		},
		{
			"create_post-with-content",
			"create_post", map[string]string{"post_title": "Hi", "post_content": "Body"},
			[]string{"wp", "post", "create", "--post_type=page", "--post_title=Hi", "--post_status=draft", "--user=1", "--allow-root", "--post_content=Body"},
		},
		{
			"list_plugins",
			"list_plugins", nil,
			[]string{"wp", "plugin", "list", "--format=json", "--allow-root"},
		},
		{
			"install_plugin",
			"install_plugin", map[string]string{"slug": "akismet"},
			[]string{"wp", "plugin", "install", "akismet", "--activate", "--allow-root"},
		},
		{
			"install_plugin-hyphenated",
			"install_plugin", map[string]string{"slug": "wp-super-cache"},
			[]string{"wp", "plugin", "install", "wp-super-cache", "--activate", "--allow-root"},
		},
		{
			"deactivate_plugin",
			"deactivate_plugin", map[string]string{"slug": "akismet"},
			[]string{"wp", "plugin", "deactivate", "akismet", "--allow-root"},
		},
		{
			"list_themes",
			"list_themes", nil,
			[]string{"wp", "theme", "list", "--format=json", "--allow-root"},
		},
		{
			"activate_theme",
			"activate_theme", map[string]string{"slug": "twentytwentyfour"},
			[]string{"wp", "theme", "activate", "twentytwentyfour", "--allow-root"},
		},
		{
			"search_replace-is-dry-run",
			"search_replace", map[string]string{"old": "foo", "new": "bar"},
			[]string{"wp", "search-replace", "foo", "bar", "--dry-run", "--allow-root"},
		},
		{
			"media_import-https",
			"media_import", map[string]string{"url": "https://example.com/img.png"},
			[]string{"wp", "media", "import", "https://example.com/img.png", "--allow-root"},
		},
		{
			"media_import-with-title",
			"media_import", map[string]string{"url": "https://example.com/img.png", "title": "Pic"},
			[]string{"wp", "media", "import", "https://example.com/img.png", "--allow-root", "--title=Pic"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildTool(c.tool, c.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalStrSlice(got, c.wantCmd) {
				t.Errorf("got %v, want %v", got, c.wantCmd)
			}
		})
	}
}

func TestBuildTool_Errors(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		params    map[string]string
		wantErr   string // substring to match
	}{
		{"unknown-tool", "wp_eval", nil, "unknown tool"},
		{"unknown-tool-blank", "", nil, "unknown tool"},

		{"get_option-empty-key", "get_option", map[string]string{"key": ""}, "required"},
		{"get_option-blocked-siteurl", "get_option", map[string]string{"key": "siteurl"}, "not allowed"},
		{"get_option-blocked-home", "get_option", map[string]string{"key": "home"}, "not allowed"},
		{"get_option-blocked-admin_email", "get_option", map[string]string{"key": "admin_email"}, "not allowed"},

		{"update_option-blocked-siteurl", "update_option", map[string]string{"key": "siteurl", "value": "x"}, "not allowed"},
		{"update_option-missing-value", "update_option", map[string]string{"key": "blogname", "value": ""}, "required"},

		{"list_posts-bad-type", "list_posts", map[string]string{"post_type": "anything"}, "post_type"},
		{"list_posts-bad-status", "list_posts", map[string]string{"status": "weird"}, "status"},

		{"get_post-missing-id", "get_post", nil, "required"},
		{"get_post-zero-id", "get_post", map[string]string{"id": "0"}, "positive integer"},
		{"get_post-negative-id", "get_post", map[string]string{"id": "-3"}, "positive integer"},
		{"get_post-non-numeric-id", "get_post", map[string]string{"id": "abc"}, "positive integer"},

		{"update_post-missing-field", "update_post", map[string]string{"id": "1"}, "required"},
		{"update_post-bad-field", "update_post", map[string]string{"id": "1", "field": "post_author", "value": "1"}, "not allowed"},

		{"create_post-missing-title", "create_post", map[string]string{"post_type": "post"}, "required"},
		{"create_post-bad-type", "create_post", map[string]string{"post_type": "movie", "post_title": "x"}, "post_type"},

		{"install_plugin-bad-slug-uppercase", "install_plugin", map[string]string{"slug": "Akismet"}, "invalid plugin slug"},
		{"install_plugin-bad-slug-special", "install_plugin", map[string]string{"slug": "wp_super_cache"}, "invalid plugin slug"},
		{"install_plugin-bad-slug-traversal", "install_plugin", map[string]string{"slug": "../etc/passwd"}, "invalid plugin slug"},
		{"deactivate_plugin-bad-slug", "deactivate_plugin", map[string]string{"slug": "WP-Super-Cache"}, "invalid plugin slug"},
		{"activate_theme-bad-slug", "activate_theme", map[string]string{"slug": "BadSlug!"}, "invalid theme slug"},

		{"search_replace-missing-old", "search_replace", map[string]string{"old": "", "new": "y"}, "required"},
		{"search_replace-missing-new", "search_replace", map[string]string{"old": "x"}, "required"},

		// SSRF guard: every blocked range must be rejected
		{"media_import-http", "media_import", map[string]string{"url": "http://example.com/x.png"}, "https"},
		{"media_import-loopback-127", "media_import", map[string]string{"url": "https://127.0.0.1/x.png"}, "public host"},
		{"media_import-loopback-name", "media_import", map[string]string{"url": "https://localhost/x.png"}, "public host"},
		{"media_import-loopback-v6", "media_import", map[string]string{"url": "https://[::1]/x.png"}, "public host"},
		{"media_import-rfc1918-10", "media_import", map[string]string{"url": "https://***REMOVED***/x.png"}, "public host"},
		{"media_import-rfc1918-192-168", "media_import", map[string]string{"url": "https://***REMOVED***/x.png"}, "public host"},
		{"media_import-rfc1918-172-16", "media_import", map[string]string{"url": "https://***REMOVED***/x.png"}, "public host"},
		{"media_import-rfc1918-172-31", "media_import", map[string]string{"url": "https://***REMOVED***/x.png"}, "public host"},
		{"media_import-link-local", "media_import", map[string]string{"url": "https://***REMOVED***/latest/"}, "public host"},
		{"media_import-empty-url", "media_import", map[string]string{"url": ""}, "required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildTool(c.tool, c.params)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestSafeOptionKeys_NoDangerousKeys(t *testing.T) {
	// These keys MUST never appear in the allowlist. If one ever does, the
	// agent can hijack the install (siteurl/home) or lock the admin out
	// (admin_email).
	forbidden := []string{
		"siteurl", "home", "admin_email", "blog_charset",
		"template", "stylesheet", "active_plugins", "secret_key",
	}
	for _, k := range forbidden {
		if safeOptionKeys[k] {
			t.Errorf("safeOptionKeys must not allow %q", k)
		}
	}
}

func TestPostFieldAllowlist_Bounded(t *testing.T) {
	// Spot-check the critical security boundary: post_author and post_password
	// must NEVER be writable. Flipping post_author hands ownership of every
	// post to whoever drives the agent; post_password silently locks readers
	// out. Other fields (post_date, etc.) are deliberately allowlisted —
	// see wp_exec.go postFieldAllowlist.
	forbidden := []string{"post_author", "post_password"}
	for _, k := range forbidden {
		if postFieldAllowlist[k] {
			t.Errorf("postFieldAllowlist must not allow %q", k)
		}
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// media_pass_state is only referenced as a switch case, so the compiler lowers
// the literal into inline comparisons — you cannot verify its presence by
// grepping the binary (that false negative cost a deploy cycle on 2026-07-09).
// This test is the reliable check.
func TestMediaPassState(t *testing.T) {
	cmd, err := buildTool("media_pass_state", map[string]string{})
	if err != nil {
		t.Fatalf("media_pass_state: %v", err)
	}
	if len(cmd) < 3 || cmd[0] != "wp" || cmd[1] != "eval" {
		t.Fatalf("unexpected command shape: %v", cmd)
	}
	for _, want := range []string{"hostplane_home_sections", "site_icon", "_thumbnail_id", "_hostplane_placeholder"} {
		if !strings.Contains(cmd[2], want) {
			t.Fatalf("snapshot php missing %s", want)
		}
	}
}

// 2026-07-09 real-user regressions: menu add-custom argument order (title
// FIRST — the swap produced "http://Home" links), and page_on_front must
// publish the page it assigns.
func TestMenuAddCustomArgOrder(t *testing.T) {
	cmd, err := buildTool("add_menu_item", map[string]string{"menu_id": "16", "title": "Home", "url": "/"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wp", "menu", "item", "add-custom", "16", "Home", "/", "--allow-root"}
	if strings.Join(cmd, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", cmd, want)
	}
}

func TestPageOnFrontPublishes(t *testing.T) {
	cmd, err := buildTool("update_option", map[string]string{"key": "page_on_front", "value": "30"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd[1] != "eval" || !strings.Contains(cmd[2], "post_status") || !strings.Contains(cmd[2], "show_on_front") {
		t.Fatalf("page_on_front should publish via eval: %v", cmd[:2])
	}
}

// 2026-07-10 founder-session regressions: entity-encoded titles must compare
// equal, and Coming Soon mode must fail acceptance by name.
func TestEntityNormalizedProductCheck(t *testing.T) {
	st := storeState{WC: true, ShowOnFront: "page", FrontID: 5, FrontStatus: "publish",
		PaymentsEnabled: 1, ShippingMethods: 1, PrivacyPage: true, TermsPage: true,
		ShopPageID: 6, ShopStatus: "publish",
		Products: []storeProduct{{Name: "Wall Mixer 3-in-1 (Bath &amp; Shower)", Price: "3499"}}}
	fails := storeChecks(st, "", []storeProduct{{Name: "Wall Mixer 3-in-1 (Bath & Shower)", Price: "3499"}})
	for _, f := range fails {
		if strings.Contains(f, "product_missing") {
			t.Fatalf("entity-encoded name should match: %v", fails)
		}
	}
}

func TestComingSoonFailsAcceptance(t *testing.T) {
	st := storeState{WC: true, ComingSoon: true, ShowOnFront: "page", FrontID: 5, FrontStatus: "publish",
		PaymentsEnabled: 1, ShippingMethods: 1, PrivacyPage: true, TermsPage: true,
		ShopPageID: 6, ShopStatus: "publish"}
	fails := storeChecks(st, "INR", nil)
	found := false
	for _, f := range fails {
		if strings.Contains(f, "coming_soon") {
			found = true
		}
	}
	if !found {
		t.Fatalf("coming-soon must fail acceptance: %v", fails)
	}
}

// Founder-reported content classes: literal markdown and bracketed
// placeholders must fail page verification.
func TestMarkdownAndPlaceholderChecks(t *testing.T) {
	body := "<html><body><p>**Email:** someone@x.com</p><p>[Contact Form Placeholder]</p></body></html>"
	fails := verifyPageChecks(body, "200", nil, nil)
	var md, ph bool
	for _, f := range fails {
		if strings.Contains(f, "markdown_visible") {
			md = true
		}
		if strings.Contains(f, "placeholder_text") {
			ph = true
		}
	}
	if !md || !ph {
		t.Fatalf("expected markdown+placeholder failures, got %v", fails)
	}
}

// F14: advertised offers must be backed by store config.
func TestAdvertisedOfferBacking(t *testing.T) {
	base := storeState{WC: true, ShowOnFront: "page", FrontID: 5, FrontStatus: "publish",
		PaymentsEnabled: 1, ShippingMethods: 1, PrivacyPage: true, TermsPage: true,
		ShopPageID: 6, ShopStatus: "publish",
		HomeSections: `[{"type":"announcement","chips":["10% off your first order","Fresh daily"]}]`}
	fails := storeChecks(base, "INR", nil)
	found := false
	for _, f := range fails {
		if strings.Contains(f, "advertised_offer_unbacked") {
			found = true
		}
	}
	if !found {
		t.Fatalf("percent-off with no coupon must fail: %v", fails)
	}
	base.CouponCount = 1
	for _, f := range storeChecks(base, "INR", nil) {
		if strings.Contains(f, "advertised_offer_unbacked") {
			t.Fatalf("backed offer should pass: %v", f)
		}
	}
}

func TestCategoryRequirementForLargeCatalogs(t *testing.T) {
	st := storeState{WC: true, ShowOnFront: "page", FrontID: 5, FrontStatus: "publish",
		PaymentsEnabled: 1, ShippingMethods: 1, PrivacyPage: true, TermsPage: true,
		ShopPageID: 6, ShopStatus: "publish"}
	var many []storeProduct
	for i := 0; i < 8; i++ {
		many = append(many, storeProduct{Name: "Product " + string(rune(65+i)), Price: "10"})
	}
	st.Products = many
	fails := storeChecks(st, "", many)
	found := false
	for _, f := range fails {
		if strings.Contains(f, "no_product_categories") {
			found = true
		}
	}
	if !found {
		t.Fatalf("8 products with 0 categories must fail: %v", fails)
	}
	st.ProductCatCount = 3
	for _, f := range storeChecks(st, "", many) {
		if strings.Contains(f, "no_product_categories") {
			t.Fatalf("categorized catalog should pass: %v", f)
		}
	}
}
