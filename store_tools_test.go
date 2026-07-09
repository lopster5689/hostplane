package main

import (
	"strings"
	"testing"
)

// w1benchBrokenState mirrors the REAL defects shipped on the w1bench store
// (2026-07-08): wrong currency, ÷100 price, http://Shop menu link, Hello-world
// left behind. These are locked as MUST-FAIL.
func w1benchBrokenState() storeState {
	return storeState{
		Currency:    "USD",
		ShowOnFront: "posts",
		FrontID:     0,
		DefaultPost: true,
		WC:          true,
		ShopPageID:  5,
		ShopStatus:  "publish",
		Products: []storeProduct{
			{Name: "Butter Croissant", Price: "1.20"},
			{Name: "Sourdough Loaf", Price: "2.50"},
		},
		MenuItems: []storeMenuItem{
			{Title: "Home", URL: "https://w1bench.cowsaidmoo.tech/"},
			{Title: "Shop", URL: "http://Shop"},
		},
	}
}

func goodStoreState() storeState {
	return storeState{
		Currency:        "INR",
		ShowOnFront:     "page",
		FrontID:         17,
		FrontStatus:     "publish",
		DefaultPost:     false,
		DefaultPage:     false,
		WC:              true,
		ShopPageID:      5,
		ShopStatus:      "publish",
		PaymentsEnabled: 1,
		ShippingMethods: 1,
		PrivacyPage:     true,
		TermsPage:       true,
		Products: []storeProduct{
			{Name: "Butter Croissant", Price: "120"},
			{Name: "Sourdough Loaf", Price: "250.00"},
		},
		MenuItems: []storeMenuItem{
			{Title: "Home", URL: "https://w1bench.cowsaidmoo.tech/"},
			{Title: "Shop", URL: "/shop/"},
		},
	}
}

func specProducts() []storeProduct {
	return []storeProduct{
		{Name: "Butter Croissant", Price: "120"},
		{Name: "Sourdough Loaf", Price: "250"},
	}
}

func TestStoreChecksFailsTheRealW1benchDefects(t *testing.T) {
	fails := storeChecks(w1benchBrokenState(), "INR", specProducts())
	joined := strings.Join(fails, "\n")
	for _, want := range []string{
		"currency_mismatch",         // $ instead of ₹
		"product_price_wrong",       // 1.20 instead of 120 (÷100)
		"menu_link_invalid",         // http://Shop
		"default_post_present",      // Hello world! not cleaned
		"homepage_not_a_page",       // storefront fell back to the blog
		"no_payment_method_enabled", // funnel floor (F2)
		"no_shipping_method",
		"legal_pages_missing",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected failure %q missing from:\n%s", want, joined)
		}
	}
}

func TestStoreChecksPassesACorrectStore(t *testing.T) {
	if fails := storeChecks(goodStoreState(), "INR", specProducts()); len(fails) != 0 {
		t.Errorf("correct store must pass, got: %v", fails)
	}
}

func TestStoreChecksSkipsStoreAssertsWithoutASpec(t *testing.T) {
	// A content-edit job (no manifest) must never be failed for pre-existing
	// site conditions it didn't touch.
	if fails := storeChecks(w1benchBrokenState(), "", nil); len(fails) != 0 {
		t.Errorf("spec-less check must be empty, got: %v", fails)
	}
}

func TestPriceEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"120", "120.00", true},
		{"250.00", "250", true},
		{"1.20", "120", false},
		{"120.1", "120.10", true},
		{"", "120", false},
		{"abc", "abc", true}, // non-numeric falls back to exact match
	}
	for _, c := range cases {
		if got := priceEqual(c.a, c.b); got != c.want {
			t.Errorf("priceEqual(%q,%q)=%t want %t", c.a, c.b, got, c.want)
		}
	}
}

func TestMenuURLValidation(t *testing.T) {
	valid := []string{
		"https://example.com", "https://example.com/shop/", "http://sub.example.co.in/x?y=1",
		"/shop/", "/", "/cart?added=1",
	}
	invalid := []string{
		"Shop", "http://Shop", "http://Shop/", "example.com", "shop/", "javascript:alert(1)",
	}
	for _, u := range valid {
		if !menuURLRe.MatchString(u) {
			t.Errorf("expected valid: %q", u)
		}
	}
	for _, u := range invalid {
		if menuURLRe.MatchString(u) {
			t.Errorf("expected INVALID: %q", u)
		}
	}
}

func TestValidateWCPrice(t *testing.T) {
	for _, ok := range []string{"120", "120.5", "120.50", "1", "9999999.99"} {
		if err := validateWCPrice("regular_price", ok); err != nil {
			t.Errorf("expected valid price %q, got %v", ok, err)
		}
	}
	for _, bad := range []string{"₹120", "$1.20", "1,200", "0", "0.00", "-5", "12.345", "", "12 0", "1e3"} {
		if err := validateWCPrice("regular_price", bad); err == nil {
			t.Errorf("expected rejection for price %q", bad)
		}
	}
}

func TestValidateHomeSections(t *testing.T) {
	good := `[{"type":"hero_image","image_id":42,"headline":"Fresh Daily","sub":"Baked at 5am"},
	{"type":"story","title":"Our Craft","text":"We bake."},
	{"type":"bestsellers","title":"Favorites"},
	{"type":"hours_location","hours":["Mon-Sat 8-6"],"address":"12 MG Road","phone":"+91 98x"},
	{"type":"offer_banner","text":"Free delivery over 500"}]`
	norm, err := validateHomeSections(good)
	if err != nil || !strings.Contains(norm, "hero_image") {
		t.Fatalf("valid sections rejected: %v", err)
	}
	bad := []string{
		`[]`,                                     // empty
		`[{"type":"marquee"}]`,                   // unknown type
		`[{"type":"hero_image","headline":"x"}]`, // hero without image
		`[{"type":"story","title":"x"}]`,         // story without text
		`[{"type":"gallery","image_ids":[1]}]`,   // gallery too small
		`[{"type":"offer_banner"}]`,              // banner without text
		`not json`,
	}
	for _, b := range bad {
		if _, err := validateHomeSections(b); err == nil {
			t.Errorf("expected rejection for %s", b)
		}
	}
}

func TestDesignChecks(t *testing.T) {
	tokens := `{"primary_color":"#9b5b32","secondary_color":"#4c4037","accent_color":"#c98a4b"}`
	goodCSS := ":root { --color-primary: #9b5b32; --color-secondary: #4c4037; --color-accent: #c98a4b; }"
	rich := `[{"type":"hero_image","image_id":9,"headline":"x"},{"type":"story","title":"a","text":"b"},{"type":"bestsellers"},{"type":"hours_location","address":"x"}]`

	// Fully-conforming build passes.
	if fails := designChecks(storeState{OurTheme: true, StyleCSS: goodCSS, HomeSections: rich}, tokens); len(fails) != 0 {
		t.Errorf("conforming design must pass, got %v", fails)
	}
	// No brief -> no design assertions (content-edit jobs unaffected).
	if fails := designChecks(storeState{OurTheme: true}, ""); fails != nil {
		t.Errorf("no-brief job must skip design checks, got %v", fails)
	}
	// Theme ignored the approved palette -> token mismatch.
	blueCSS := ":root { --color-primary: #2563eb; --color-secondary: #10b981; --color-accent: #f59e0b; }"
	fails := designChecks(storeState{OurTheme: true, StyleCSS: blueCSS, HomeSections: rich}, tokens)
	if len(fails) < 3 || !strings.Contains(strings.Join(fails, " "), "design_token_mismatch") {
		t.Errorf("ignored brief must fail all three tokens, got %v", fails)
	}
	// AI-default indigo anywhere in the css -> slop failure.
	slop := designChecks(storeState{OurTheme: true, StyleCSS: goodCSS + " .x{color:#6366f1}", HomeSections: rich}, tokens)
	if !strings.Contains(strings.Join(slop, " "), "design_slop_color") {
		t.Errorf("slop hex must fail, got %v", slop)
	}
	// Thin homepage (legacy 0 sections) and hero-less compositions fail.
	thin := designChecks(storeState{OurTheme: true, StyleCSS: goodCSS, HomeSections: ""}, tokens)
	if !strings.Contains(strings.Join(thin, " "), "homepage_too_thin") {
		t.Errorf("sectionless homepage must fail, got %v", thin)
	}
	noHero := `[{"type":"story","title":"a","text":"b"},{"type":"bestsellers"},{"type":"offer_banner","text":"x"},{"type":"category_strip"}]`
	nh := designChecks(storeState{OurTheme: true, StyleCSS: goodCSS, HomeSections: noHero}, tokens)
	if !strings.Contains(strings.Join(nh, " "), "homepage_missing_hero_image") {
		t.Errorf("hero-less homepage must fail, got %v", nh)
	}
}

func TestValidateHomeSectionsV3Types(t *testing.T) {
	good := `[{"type":"announcement","chips":["Free delivery over 500","Baked fresh daily"]},
	{"type":"hero_image","image_id":7,"eyebrow":"100% WOOD-FIRED","headline":"Baked with Fire","accent_word":"Fire","chips":["Fresh daily"]},
	{"type":"testimonials","items":[{"quote":"Best croissants in town","name":"Asha","meta":"Indiranagar"},{"quote":"The sourdough!","name":"Rahul"}]},
	{"type":"bestsellers"}]`
	if _, err := validateHomeSections(good); err != nil {
		t.Fatalf("v3 sections rejected: %v", err)
	}
	bad := []string{
		`[{"type":"announcement","chips":["only one"]}]`,                              // too few chips
		`[{"type":"testimonials","items":[{"quote":"x","name":"y"}]}]`,               // too few items
		`[{"type":"testimonials","items":[{"quote":"","name":"y"},{"quote":"b","name":""}]}]`, // empty fields
		`[{"type":"hero_image","image_id":7,"headline":"Baked with Fire","accent_word":"Love"}]`, // accent word not in headline
	}
	for _, b := range bad {
		if _, err := validateHomeSections(b); err == nil {
			t.Errorf("expected rejection for %s", b)
		}
	}
}
