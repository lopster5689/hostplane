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
		Currency:    "INR",
		ShowOnFront: "page",
		FrontID:     17,
		FrontStatus: "publish",
		DefaultPost: false,
		DefaultPage: false,
		WC:          true,
		ShopPageID:  5,
		ShopStatus:  "publish",
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
		"currency_mismatch",     // $ instead of ₹
		"product_price_wrong",   // 1.20 instead of 120 (÷100)
		"menu_link_invalid",     // http://Shop
		"default_post_present",  // Hello world! not cleaned
		"homepage_not_a_page",   // storefront fell back to the blog
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
		`[]`, // empty
		`[{"type":"marquee"}]`,                                  // unknown type
		`[{"type":"hero_image","headline":"x"}]`,                // hero without image
		`[{"type":"story","title":"x"}]`,                        // story without text
		`[{"type":"gallery","image_ids":[1]}]`,                  // gallery too small
		`[{"type":"offer_banner"}]`,                             // banner without text
		`not json`,
	}
	for _, b := range bad {
		if _, err := validateHomeSections(b); err == nil {
			t.Errorf("expected rejection for %s", b)
		}
	}
}
