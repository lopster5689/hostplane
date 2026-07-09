package main

// Store-quality tooling (hosto-infra/AI-EDITOR-V2-QUALITY-PLAN.md, layers 1+3).
//
//   - prepare_store: the deterministic store baseline — currency, default-content
//     cleanup, pretty permalinks, WooCommerce pages. The model CHOOSES the
//     currency; this code performs the writes (P1). Idempotent.
//   - verify_store: the acceptance suite that decides whether a build is done —
//     real DB facts + rendered pages checked against the job's typed manifest
//     (P4). The dashboard's finalizeJob runs this before any job can settle;
//     the model can also call it read-only to preview what would fail.
//
// Check logic is split into pure functions (storeChecks, priceEqual) so the
// exact bug classes that shipped — ÷100 prices, $-instead-of-₹, http://Shop
// menu links, Hello-world left behind — are locked out by unit tests.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// storeCurrencies mirrors the dashboard's supported set (agent_jobs.go) — keep
// both in sync when extending.
var storeCurrencies = map[string]bool{
	"INR": true, "USD": true, "EUR": true, "GBP": true, "AUD": true,
	"CAD": true, "SGD": true, "AED": true, "JPY": true, "NZD": true,
}

// currencyMarkers: strings whose presence in rendered product HTML shows the
// price actually renders in the expected currency (glyph or HTML entity); the
// WooCommerce price-markup class is accepted as a structural fallback.
var currencyMarkers = map[string][]string{
	"INR": {"₹", "&#8377;", "&#x20b9;"},
	"USD": {"$"},
	"EUR": {"€", "&euro;", "&#8364;"},
	"GBP": {"£", "&pound;", "&#163;"},
	"AUD": {"$"},
	"CAD": {"$"},
	"SGD": {"$"},
	"AED": {"د.إ", "AED"},
	"JPY": {"¥", "&yen;", "&#165;"},
	"NZD": {"$"},
}

// handlePrepareStore performs the deterministic store baseline. Everything in
// here is config the model used to "wire" by hand (and got wrong): the store
// currency, the stock Hello-world/Sample-Page/first-comment cleanup, pretty
// permalinks (without which /shop and product URLs break), and the standard
// WooCommerce pages. Safe to run repeatedly.
func (a *API) handlePrepareStore(site string, params map[string]string) (string, int, error) {
	currency := strings.ToUpper(strings.TrimSpace(params["currency"]))
	if !storeCurrencies[currency] {
		return "", -1, fmt.Errorf("currency is required and must be one of INR, USD, EUR, GBP, AUD, CAD, SGD, AED, JPY, NZD")
	}
	// Free-text values ride base64 so they can't break out of the PHP string.
	nameB64 := base64.StdEncoding.EncodeToString([]byte(params["store_name"]))
	tagB64 := base64.StdEncoding.EncodeToString([]byte(params["tagline"]))
	phpCode := `
		$done = array();
		update_option('woocommerce_currency', '` + currency + `');
		$done[] = 'currency=` + currency + `';
		$name = base64_decode("` + nameB64 + `");
		if ($name !== '') { update_option('blogname', wp_strip_all_tags($name)); $done[] = 'store name set'; }
		$tag = base64_decode("` + tagB64 + `");
		if ($tag !== '') { update_option('blogdescription', wp_strip_all_tags($tag)); $done[] = 'tagline set'; }
		foreach (get_posts(array('name' => 'hello-world', 'post_type' => 'post', 'post_status' => 'any', 'numberposts' => 1)) as $p) {
			wp_delete_post($p->ID, true); $done[] = 'deleted default post';
		}
		$sample = get_page_by_path('sample-page', OBJECT, 'page');
		if ($sample) { wp_delete_post($sample->ID, true); $done[] = 'deleted sample page'; }
		foreach (get_comments(array('author_email' => 'wapuu@wordpress.example', 'number' => 5)) as $c) {
			wp_delete_comment($c->comment_ID, true); $done[] = 'deleted default comment';
		}
		if (get_option('permalink_structure') !== '/%postname%/') {
			update_option('permalink_structure', '/%postname%/');
			flush_rewrite_rules();
			$done[] = 'permalinks=/%postname%/';
		}
		if (class_exists('WC_Install')) {
			WC_Install::create_pages();
			$done[] = 'woocommerce pages ensured';
		}
		// F3: the block-based cart/checkout hydrate client-side and stick as
		// skeletons in Tailwind-preflight themes (observed live). The classic
		// shortcodes render server-side — reliable everywhere — and style.css
		// ships token styling for them.
		if (function_exists('wc_get_page_id')) {
			foreach (array('cart' => '[woocommerce_cart]', 'checkout' => '[woocommerce_checkout]') as $k => $sc) {
				$pid = (int) wc_get_page_id($k);
				if ($pid > 0) {
					$pg = get_post($pid);
					if ($pg && strpos($pg->post_content, 'wp:woocommerce/' . $k) !== false) {
						wp_update_post(array('ID' => $pid, 'post_content' => $sc));
						$done[] = $k . ' switched to classic (server-rendered)';
					}
				}
			}
		}
		// Funnel floor (design-depth F1): a store a stranger can actually buy
		// from. Country from currency; COD enabled as the deterministic
		// baseline gateway (owner upgrades to Razorpay/UPI later); one
		// everywhere shipping method; legal pages checkout links point at.
		update_option('woocommerce_default_country', '` + currencyCountry(currency) + `');
		update_option('woocommerce_default_customer_address', 'base');
		$done[] = 'store country=` + currencyCountry(currency) + `';
		if (class_exists('WooCommerce')) {
			$cod = get_option('woocommerce_cod_settings', array());
			if (!is_array($cod)) { $cod = array(); }
			if (!isset($cod['enabled']) || $cod['enabled'] !== 'yes') {
				$cod = array_merge(array('title' => 'Cash on delivery', 'description' => 'Pay with cash upon delivery.'), $cod);
				$cod['enabled'] = 'yes';
				update_option('woocommerce_cod_settings', $cod);
				$done[] = 'cash-on-delivery enabled';
			}
			if (class_exists('WC_Shipping_Zone')) {
				$rest = new WC_Shipping_Zone(0);
				if (count($rest->get_shipping_methods()) === 0) {
					$rest->add_shipping_method('flat_rate');
					$done[] = 'shipping method added (flat rate, everywhere)';
				}
			}
		}
		$priv = (int) get_option('wp_page_for_privacy_policy');
		if ($priv <= 0 || get_post_status($priv) !== 'publish') {
			$pid = wp_insert_post(array('post_title' => 'Privacy Policy', 'post_type' => 'page', 'post_status' => 'publish',
				'post_content' => 'We collect only the information needed to process your order (name, address, contact). We never sell your data. Contact us to review or delete your information.'));
			if ($pid && !is_wp_error($pid)) { update_option('wp_page_for_privacy_policy', $pid); $done[] = 'privacy policy page created'; }
		}
		$terms = (int) get_option('woocommerce_terms_page_id');
		if ($terms <= 0 || get_post_status($terms) !== 'publish') {
			$tid = wp_insert_post(array('post_title' => 'Terms and Conditions', 'post_type' => 'page', 'post_status' => 'publish',
				'post_content' => 'Orders are confirmed on payment or on delivery for cash orders. Contact us for cancellations, returns and refunds; we will make it right.'));
			if ($tid && !is_wp_error($tid)) { update_option('woocommerce_terms_page_id', $tid); $done[] = 'terms page created'; }
		}
		echo 'prepare_store: ' . implode('; ', $done);
	`
	return a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(phpCode) + " --allow-root 2>&1"})
}

// currencyCountry maps a store currency to the WooCommerce base country the
// funnel floor sets (checkout defaults, tax/shipping logic). Deliberately the
// obvious mapping; owners with multi-country stores adjust in admin.
func currencyCountry(currency string) string {
	m := map[string]string{
		"INR": "IN", "USD": "US", "EUR": "DE", "GBP": "GB", "AUD": "AU",
		"CAD": "CA", "SGD": "SG", "AED": "AE", "JPY": "JP", "NZD": "NZ",
	}
	if c, ok := m[currency]; ok {
		return c
	}
	return "US"
}

// storeStatePHP collects the acceptance facts in one wp-eval round trip.
const storeStatePHP = `
	$out = array();
	$out['currency'] = (string) get_option('woocommerce_currency');
	$out['show_on_front'] = (string) get_option('show_on_front');
	$front = (int) get_option('page_on_front');
	$out['front_id'] = $front;
	$out['front_status'] = $front ? (string) get_post_status($front) : '';
	$out['permalinks'] = (string) get_option('permalink_structure');
	$hello = get_posts(array('name' => 'hello-world', 'post_type' => 'post', 'post_status' => 'publish', 'numberposts' => 1));
	$out['default_post'] = !empty($hello);
	$sample = get_page_by_path('sample-page', OBJECT, 'page');
	$out['default_page'] = $sample && $sample->post_status === 'publish';
	$out['wc'] = class_exists('WooCommerce');
	$out['our_theme'] = file_exists(get_stylesheet_directory() . '/.hostplane-scaffold');
	$out['home_sections'] = (string) get_option('hostplane_home_sections', '');
	$css = @file_get_contents(get_stylesheet_directory() . '/style.css');
	$out['style_css'] = $css === false ? '' : substr($css, 0, 8192);
	$out['products'] = array();
	$out['shop_page'] = 0; $out['shop_status'] = ''; $out['shop_path'] = ''; $out['product_path'] = '';
	if ($out['wc']) {
		$ps = wc_get_products(array('limit' => 100, 'status' => 'publish'));
		foreach ($ps as $p) {
			$out['products'][] = array('name' => $p->get_name(), 'price' => (string) $p->get_regular_price());
		}
		if (!empty($ps)) { $out['product_path'] = wp_make_link_relative(get_permalink($ps[0]->get_id())); }
		$shop = (int) wc_get_page_id('shop');
		$out['shop_page'] = $shop;
		if ($shop > 0) {
			$out['shop_status'] = (string) get_post_status($shop);
			$out['shop_path'] = wp_make_link_relative(get_permalink($shop));
		}
	}
	$out['payments_enabled'] = 0;
	$out['shipping_methods'] = 0;
	if ($out['wc'] && function_exists('WC') && WC()->payment_gateways()) {
		$out['payments_enabled'] = count(WC()->payment_gateways()->get_available_payment_gateways());
		if (class_exists('WC_Shipping_Zones')) {
			$n = 0;
			foreach (WC_Shipping_Zones::get_zones() as $z) { $n += count($z['shipping_methods']); }
			$rest = new WC_Shipping_Zone(0);
			$n += count($rest->get_shipping_methods());
			$out['shipping_methods'] = $n;
		}
	}
	$priv = (int) get_option('wp_page_for_privacy_policy');
	$out['privacy_page'] = $priv > 0 && get_post_status($priv) === 'publish';
	$termsp = (int) get_option('woocommerce_terms_page_id');
	$out['terms_page'] = $termsp > 0 && get_post_status($termsp) === 'publish';
	$out['menu_items'] = array();
	foreach (wp_get_nav_menus() as $menu) {
		$items = wp_get_nav_menu_items($menu->term_id);
		if (!$items) { continue; }
		foreach ($items as $it) {
			$out['menu_items'][] = array('title' => (string) $it->title, 'url' => (string) $it->url);
		}
	}
	echo json_encode($out);
`

// storeState is the parsed acceptance snapshot.
type storeState struct {
	Currency        string          `json:"currency"`
	ShowOnFront     string          `json:"show_on_front"`
	FrontID         int             `json:"front_id"`
	FrontStatus     string          `json:"front_status"`
	Permalinks      string          `json:"permalinks"`
	DefaultPost     bool            `json:"default_post"`
	DefaultPage     bool            `json:"default_page"`
	WC              bool            `json:"wc"`
	OurTheme        bool            `json:"our_theme"`
	HomeSections    string          `json:"home_sections"`
	StyleCSS        string          `json:"style_css"`
	PaymentsEnabled int             `json:"payments_enabled"`
	ShippingMethods int             `json:"shipping_methods"`
	PrivacyPage     bool            `json:"privacy_page"`
	TermsPage       bool            `json:"terms_page"`
	ShopPageID      int             `json:"shop_page"`
	ShopStatus      string          `json:"shop_status"`
	ShopPath        string          `json:"shop_path"`
	ProductPath     string          `json:"product_path"`
	Products        []storeProduct  `json:"products"`
	MenuItems       []storeMenuItem `json:"menu_items"`
}

type storeProduct struct {
	Name  string `json:"name"`
	Price string `json:"price"`
}

type storeMenuItem struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// priceEqual compares WooCommerce price strings numerically ("120" == "120.00");
// non-numeric strings fall back to exact match.
func priceEqual(a, b string) bool {
	fa, ea := strconv.ParseFloat(strings.TrimSpace(a), 64)
	fb, eb := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if ea != nil || eb != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return math.Abs(fa-fb) < 0.005
}

// storeChecks computes the DB-level acceptance failures. Pure and unit-tested.
// The store-specific assertions apply only when the job declared a store spec
// (currency and/or products) — a plain content-edit job is never failed for
// pre-existing site conditions it didn't touch.
func storeChecks(st storeState, expectCurrency string, expectProducts []storeProduct) []string {
	var fails []string
	if expectCurrency == "" && len(expectProducts) == 0 {
		return fails
	}
	if expectCurrency != "" && !strings.EqualFold(strings.TrimSpace(st.Currency), expectCurrency) {
		fails = append(fails, fmt.Sprintf("currency_mismatch: store=%q expected=%q", st.Currency, expectCurrency))
	}
	if st.ShowOnFront != "page" || st.FrontID <= 0 {
		fails = append(fails, "homepage_not_a_page: show_on_front="+st.ShowOnFront)
	} else if st.FrontStatus != "publish" {
		fails = append(fails, "homepage_not_published: status="+st.FrontStatus)
	}
	if st.DefaultPost {
		fails = append(fails, "default_post_present: the stock Hello-world post is still published")
	}
	if st.DefaultPage {
		fails = append(fails, "default_sample_page_present")
	}
	if !st.WC {
		fails = append(fails, "woocommerce_not_active")
	} else {
		if st.ShopPageID <= 0 || st.ShopStatus != "publish" {
			fails = append(fails, "shop_page_missing_or_unpublished (run prepare_store)")
		}
		// Funnel floor (F2): a store nobody can pay or get delivery from is
		// not done, however good it looks. prepare_store fixes all three.
		if st.PaymentsEnabled < 1 {
			fails = append(fails, "no_payment_method_enabled (prepare_store enables cash-on-delivery as the baseline)")
		}
		if st.ShippingMethods < 1 {
			fails = append(fails, "no_shipping_method (prepare_store adds a flat-rate everywhere zone)")
		}
		if !st.PrivacyPage || !st.TermsPage {
			fails = append(fails, "legal_pages_missing (prepare_store creates Privacy Policy + Terms)")
		}
	}
	for _, want := range expectProducts {
		var got string
		found := false
		for _, p := range st.Products {
			if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(want.Name)) {
				found, got = true, p.Price
				break
			}
		}
		if !found {
			fails = append(fails, "product_missing_or_unpublished: "+want.Name)
			continue
		}
		if !priceEqual(got, want.Price) {
			fails = append(fails, fmt.Sprintf("product_price_wrong: %s is %q but the approved spec says %q", want.Name, got, want.Price))
		}
	}
	seenTitles := map[string]bool{}
	for _, it := range st.MenuItems {
		if it.URL == "" || !menuURLRe.MatchString(it.URL) {
			fails = append(fails, fmt.Sprintf("menu_link_invalid: %q -> %q (add internal pages by post_id)", it.Title, it.URL))
		}
		key := strings.ToLower(strings.TrimSpace(it.Title))
		if key != "" && seenTitles[key] {
			fails = append(fails, fmt.Sprintf("menu_item_duplicated: %q appears twice (delete one with delete_menu_item)", it.Title))
		}
		seenTitles[key] = true
	}
	return fails
}

// briefTokens is the design contract verify_store asserts (design-depth D4):
// the values the user approved at start_job must be the values the live theme
// actually declares.
type briefTokens struct {
	Primary   string `json:"primary_color"`
	Secondary string `json:"secondary_color"`
	Accent    string `json:"accent_color"`
}

// slopHexes: palette values that read as "no one designed this" (open-design's
// anti-slop linter tells). Presence in a scaffolded theme's style.css fails
// design acceptance regardless of the brief.
var slopHexes = []string{"#6366f1", "#7c3aed", "#8b5cf6"}

// designChecks computes design-acceptance failures (pure, unit-tested). Only
// runs when the job carried a design brief (tokensJSON non-empty) — a plain
// content job is never failed on design. Section checks apply to scaffolded
// themes, where set_home_sections is available by construction.
func designChecks(st storeState, tokensJSON string) []string {
	if tokensJSON == "" || tokensJSON == "{}" {
		return nil
	}
	var fails []string
	var tk briefTokens
	if err := json.Unmarshal([]byte(tokensJSON), &tk); err != nil {
		return []string{"design_tokens_unparseable"}
	}
	cssLow := strings.ToLower(st.StyleCSS)
	for name, hex := range map[string]string{"primary": tk.Primary, "secondary": tk.Secondary, "accent": tk.Accent} {
		if hex == "" {
			continue
		}
		if !strings.Contains(cssLow, "--color-"+name+": "+strings.ToLower(hex)) {
			fails = append(fails, fmt.Sprintf("design_token_mismatch: --color-%s is not the approved %s (create_theme must receive the brief's exact values)", name, hex))
		}
	}
	for _, bad := range slopHexes {
		if strings.Contains(cssLow, strings.TrimPrefix(bad, "#")) {
			fails = append(fails, "design_slop_color: "+bad+" (AI-default indigo/violet family) in style.css")
		}
	}
	if st.OurTheme {
		var secs []homeSection
		if st.HomeSections == "" || json.Unmarshal([]byte(st.HomeSections), &secs) != nil || len(secs) < 4 {
			fails = append(fails, fmt.Sprintf("homepage_too_thin: %d typed sections (need ≥4 via set_home_sections — hero_image + story + bestsellers at minimum)", len(secs)))
		} else {
			hasHero := false
			for _, s := range secs {
				if s.Type == "hero_image" && s.ImageID > 0 {
					hasHero = true
					break
				}
			}
			if !hasHero {
				fails = append(fails, "homepage_missing_hero_image (generate a wide hero image and lead with a hero_image section)")
			}
		}
	}
	return fails
}

// handleVerifyStore runs the full acceptance suite: parse expectations, collect
// DB facts (Phase A), check them (pure), then render the homepage, shop and one
// product page through the site's own nginx (Phase B) — the DOM can be right
// while the page white-screens, and vice versa. Output contract matches
// verify_page (": OK" / "FAILING") so the dashboard can gate settlement on it.
func (a *API) handleVerifyStore(site string, params map[string]string) (string, int, error) {
	expectCurrency := strings.ToUpper(strings.TrimSpace(params["currency"]))
	if expectCurrency != "" && !storeCurrencies[expectCurrency] {
		return "", -1, fmt.Errorf("unknown currency %q", expectCurrency)
	}
	var expectProducts []storeProduct
	if pj := params["products"]; pj != "" {
		if err := json.Unmarshal([]byte(pj), &expectProducts); err != nil {
			return "", -1, fmt.Errorf("products must be a JSON array of {name, price}: %w", err)
		}
	}
	storeBuild := expectCurrency != "" || len(expectProducts) > 0

	// Phase A: real DB state. If wp eval itself fails, WordPress core is not
	// installed or not working — the most fundamental acceptance failure
	// (the w1bench/666gg "site was never installed" trap).
	out, exit, err := a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(storeStatePHP) + " --allow-root 2>&1"})
	if err != nil {
		return "", -1, err
	}
	var st storeState
	parsed := false
	if i := strings.Index(out, `{"`); exit == 0 && i >= 0 {
		parsed = json.Unmarshal([]byte(strings.TrimSpace(out[i:])), &st) == nil
	}
	if !parsed {
		first := strings.TrimSpace(out)
		if n := strings.IndexByte(first, '\n'); n > 0 {
			first = first[:n]
		}
		return "verify_store: FAILING\nchecks_failed=[core_not_working: " + first + "]", 1, nil
	}

	fails := storeChecks(st, expectCurrency, expectProducts)
	fails = append(fails, designChecks(st, params["tokens"])...)

	// Phase B: render checks.
	type renderCheck struct{ path, mustContain string }
	checks := []renderCheck{{"/", ""}}
	if p := params["path"]; p != "" && p != "/" && verifyPagePathRe.MatchString(p) {
		checks = append(checks, renderCheck{p, ""})
	}
	shopRenderPath := ""
	if storeBuild && st.ShopPath != "" && verifyPagePathRe.MatchString(st.ShopPath) {
		must := ""
		if len(expectProducts) > 0 {
			must = expectProducts[0].Name
		}
		checks = append(checks, renderCheck{st.ShopPath, must})
		shopRenderPath = st.ShopPath
	}
	priceMarkerPath := ""
	if storeBuild && st.ProductPath != "" && st.ProductPath != "/" && verifyPagePathRe.MatchString(st.ProductPath) {
		checks = append(checks, renderCheck{st.ProductPath, ""})
		priceMarkerPath = st.ProductPath
	}
	for _, rc := range checks {
		body, status, _, ferr := a.fetchSitePage(site, rc.path)
		if ferr != nil {
			fails = append(fails, rc.path+": render_error")
			continue
		}
		var expect []string
		if rc.mustContain != "" {
			expect = []string{rc.mustContain}
		}
		for _, f := range verifyPageChecks(body, status, expect, nil) {
			// Stock-content render check only counts for declared store
			// builds — see storeChecks' scoping rationale.
			if !storeBuild && strings.HasPrefix(f, "default_wp_content") {
				continue
			}
			fails = append(fails, rc.path+": "+f)
		}
		// On OUR scaffolded themes the shop page must render the theme's
		// styled grid, not WooCommerce's bare ul.products loop (which needs
		// wc CSS the Tailwind preflight clobbers → the collapsed-grid bug).
		// A model-written woocommerce.php shadowing archive-product.php is
		// the known way this regresses; catch it mechanically.
		if rc.path == shopRenderPath && st.OurTheme &&
			strings.Contains(body, `<ul class="products`) && !strings.Contains(body, "grid-cols") {
			fails = append(fails, rc.path+": shop_grid_unstyled (WooCommerce default loop is rendering — a woocommerce.php is likely shadowing the theme's archive-product.php grid; remove it)")
		}
		// The product page must show a price in the expected currency —
		// this is the render-level half of the $-instead-of-₹ bug.
		if rc.path == priceMarkerPath && expectCurrency != "" {
			ok := strings.Contains(body, "woocommerce-Price-amount")
			low := strings.ToLower(body)
			for _, m := range currencyMarkers[expectCurrency] {
				if strings.Contains(low, strings.ToLower(m)) {
					ok = true
					break
				}
			}
			if !ok {
				fails = append(fails, rc.path+": price_not_rendered_in_"+expectCurrency)
			}
		}
	}

	verdict, exitCode := "OK", 0
	if len(fails) > 0 {
		verdict, exitCode = "FAILING", 1
	}
	facts := fmt.Sprintf(
		"currency=%s show_on_front=%s front=%d(%s) permalinks=%q products=%d shop_page=%d(%s) menu_items=%d",
		st.Currency, st.ShowOnFront, st.FrontID, st.FrontStatus, st.Permalinks,
		len(st.Products), st.ShopPageID, st.ShopStatus, len(st.MenuItems))
	return fmt.Sprintf("verify_store: %s\n%s\nchecks_failed=%v", verdict, facts, fails), exitCode, nil
}
