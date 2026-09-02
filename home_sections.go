package main

// Home-section library (design-depth plan D3). The homepage stops being a
// fixed 3-block template: the model COMPOSES typed sections (content only);
// the scaffold's front-page.php renders each type with deterministic
// token-class markup. Same split as everywhere else in the quality plan —
// the model chooses, code performs.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// homeSection is one typed slot. Exactly one Type; only the fields that type
// uses are read. Unknown fields are dropped by re-marshalling.
type homeSection struct {
	Type     string `json:"type"`
	Headline string `json:"headline,omitempty"`
	// AccentWord: one word of the headline rendered in the accent color
	// (the editorial "Nature's GOLD Standard" device).
	AccentWord string   `json:"accent_word,omitempty"`
	Eyebrow    string   `json:"eyebrow,omitempty"`
	Sub        string   `json:"sub,omitempty"`
	Title      string   `json:"title,omitempty"`
	Text       string   `json:"text,omitempty"`
	ImageID    int      `json:"image_id,omitempty"`
	ImageIDs   []int    `json:"image_ids,omitempty"`
	Hours      []string `json:"hours,omitempty"`
	Address    string   `json:"address,omitempty"`
	Phone      string   `json:"phone,omitempty"`
	Email      string   `json:"email,omitempty"`
	WhatsApp   string   `json:"whatsapp,omitempty"`
	Instagram  string   `json:"instagram,omitempty"`
	// Layout: hero variant — "full" (default, image behind text) or "split"
	// (content left on warm surface, framed image right — the editorial look).
	Layout string `json:"layout,omitempty"`
	// ButtonLabel: cta_band button text (defaults to "Shop Now").
	ButtonLabel string `json:"button_label,omitempty"`
	// Chips: short trust claims floated on the hero ("Lab Tested") or
	// ticked across the announcement bar ("Free delivery over ₹999").
	Chips []string `json:"chips,omitempty"`
	// Items: testimonials — quote/name/meta only; NEVER fabricated, the
	// prompt requires them to come from the user.
	Items []testimonialItem `json:"items,omitempty"`
}

type testimonialItem struct {
	Quote string `json:"quote"`
	Name  string `json:"name"`
	Meta  string `json:"meta,omitempty"`
}

var homeSectionTypes = map[string]bool{
	"hero_image": true, "story": true, "category_strip": true,
	"bestsellers": true, "hours_location": true, "gallery": true,
	"offer_banner": true, "announcement": true, "testimonials": true,
	"cta_band": true,
}

// sectionLayouts is the per-type layout allowlist (design-variance v3, §2).
// Before v3 only hero_image varied (full/split) and every other type had one
// hardcoded shape, so composition order was the ONLY thing that could differ
// between two stores — and the prompt dictated that too. First entry is the
// pre-v3 rendering, so an unspecified layout is not a redesign.
//
// announcement and offer_banner are deliberately absent: both are single-line
// bars with nothing to vary.
var sectionLayouts = map[string][]string{
	"hero_image":     {"full", "split", "stacked-editorial", "full-bleed-quiet"},
	"story":          {"text-only", "image-left", "image-right", "offset-frame"},
	"bestsellers":    {"grid-4", "grid-3-large", "asymmetric-feature", "carousel-row"},
	"category_strip": {"pills", "tiles", "banner-row"},
	"gallery":        {"grid", "masonry", "strip"},
	"testimonials":   {"cards", "single-large"},
	"cta_band":       {"solid", "image-bleed", "split-rule"},
	"hours_location": {"card", "map-side", "inline-bar"},
}

// layoutFamily groups layouts by the SHAPE they put on the page. The monotony
// that reads as "templated" is three full-width centred stacks in a row, not a
// repeated section type — so the composition rule below counts families.
var layoutFamily = map[string]string{
	"hero_image/full":             "full-bleed",
	"hero_image/full-bleed-quiet": "full-bleed",
	"cta_band/image-bleed":        "full-bleed",
	"offer_banner/":               "full-bleed",

	"hero_image/split":               "split",
	"story/image-left":               "split",
	"story/image-right":              "split",
	"story/offset-frame":             "split",
	"hours_location/map-side":        "split",
	"hours_location/card":            "split",
	"bestsellers/asymmetric-feature": "split",

	"hero_image/stacked-editorial": "stack",
	"story/text-only":              "stack",
	"cta_band/solid":               "stack",
	"testimonials/single-large":    "stack",

	"bestsellers/grid-4":       "grid",
	"bestsellers/grid-3-large": "grid",
	"gallery/grid":             "grid",
	"gallery/masonry":          "grid",
	"testimonials/cards":       "grid",

	"category_strip/pills":      "strip",
	"category_strip/tiles":      "strip",
	"category_strip/banner-row": "strip",
	"gallery/strip":             "strip",
	"bestsellers/carousel-row":  "strip",
	"hours_location/inline-bar": "strip",
	"cta_band/split-rule":       "strip",
	"announcement/":             "strip",
}

// productSurfaces: a storefront homepage has to actually show something for
// sale. Either of these counts.
var productSurfaces = map[string]bool{"bestsellers": true, "category_strip": true}

// storefrontMinSections is the size at which a composition is treated as a
// full storefront homepage and the shape constraints below apply. Under it the
// call is a partial edit ("put a hero and a story on my homepage") and only
// per-section validation runs.
//
// 6 matches the threshold the agent prompt already uses for "a real
// storefront" — the validator and the prompt must agree on where a homepage
// stops being an edit, or the model gets rejected for following instructions.
const storefrontMinSections = 6

// resolveLayout picks the concrete layout for a section, defaulting to the
// pre-v3 rendering. story is the one type whose default depends on content:
// with an image it was a two-column editorial block, without one a centred
// text well.
func resolveLayout(s homeSection) (string, error) {
	allowed, varies := sectionLayouts[s.Type]
	if !varies {
		if s.Layout != "" {
			return "", fmt.Errorf("%s takes no layout", s.Type)
		}
		return "", nil
	}
	if s.Layout == "" {
		if s.Type == "story" && s.ImageID > 0 {
			return "image-left", nil
		}
		return allowed[0], nil
	}
	for _, a := range allowed {
		if a == s.Layout {
			return a, nil
		}
	}
	return "", fmt.Errorf("layout %q is not available for %s — choose one of: %s",
		s.Layout, s.Type, strings.Join(allowed, ", "))
}

// validateComposition enforces the shape rules that replaced the prompt's
// fixed section recipe. The recipe produced identical skeletons across every
// store because it named an ORDER; these name requirements instead, leaving
// the order to the business.
func validateComposition(secs []homeSection) error {
	if len(secs) < storefrontMinSections {
		return nil
	}
	// The first section a visitor sees has to be the hero. announcement is
	// exempt — header.php lifts it out of the flow and renders it site-wide.
	first := 0
	for first < len(secs) && secs[first].Type == "announcement" {
		first++
	}
	if first >= len(secs) || secs[first].Type != "hero_image" {
		return fmt.Errorf("a storefront homepage must open with hero_image (announcement may precede it — it renders site-wide in the header)")
	}
	hasProduct, hasClose := false, false
	for _, s := range secs {
		if productSurfaces[s.Type] {
			hasProduct = true
		}
		if s.Type == "cta_band" {
			hasClose = true
		}
	}
	if !hasProduct {
		return fmt.Errorf("a storefront homepage must show something for sale — include bestsellers or category_strip")
	}
	if !hasClose {
		return fmt.Errorf("a storefront homepage must include a cta_band (the closing push)")
	}
	// Three consecutive sections of one shape is the "templated" tell.
	run, prev := 1, ""
	for _, s := range secs {
		fam := layoutFamily[s.Type+"/"+s.Layout]
		if fam == "" {
			continue
		}
		if fam == prev {
			run++
			if run >= 3 {
				return fmt.Errorf("three %s sections in a row reads as a template — vary the layout of one of them", fam)
			}
		} else {
			run, prev = 1, fam
		}
	}
	return nil
}

// validateHomeSections parses and validates the typed section list. Pure and
// unit-tested. Returns the normalized JSON to store.
func validateHomeSections(raw string) (string, error) {
	var secs []homeSection
	if err := json.Unmarshal([]byte(raw), &secs); err != nil {
		return "", fmt.Errorf("sections must be a JSON array of typed section objects: %w", err)
	}
	if len(secs) < 1 || len(secs) > 10 {
		return "", fmt.Errorf("between 1 and 10 sections (got %d)", len(secs))
	}
	capStr := func(field, v string, max int) error {
		if len(v) > max {
			return fmt.Errorf("%s too long (max %d chars)", field, max)
		}
		return nil
	}
	for i, s := range secs {
		if !homeSectionTypes[s.Type] {
			return "", fmt.Errorf("sections[%d].type %q unknown — use hero_image, story, category_strip, bestsellers, hours_location, gallery, offer_banner", i, s.Type)
		}
		// Layout is resolved to a concrete value here so front-page.php can
		// branch on it directly and the composition rules below can read the
		// shape of a section that left it unspecified.
		lay, lerr := resolveLayout(s)
		if lerr != nil {
			return "", fmt.Errorf("sections[%d]: %w", i, lerr)
		}
		secs[i].Layout, s.Layout = lay, lay
		var err error
		switch s.Type {
		case "hero_image":
			if s.ImageID <= 0 {
				return "", fmt.Errorf("sections[%d] hero_image needs image_id (generate a wide hero image first, use its attachment ID)", i)
			}
			if err = capStr("headline", s.Headline, 80); err == nil {
				err = capStr("sub", s.Sub, 200)
			}
			if err == nil {
				err = capStr("eyebrow", s.Eyebrow, 40)
			}
			if err == nil && len(s.Chips) > 3 {
				err = fmt.Errorf("max 3 hero chips")
			}
			if err == nil && s.AccentWord != "" && !strings.Contains(strings.ToLower(s.Headline), strings.ToLower(s.AccentWord)) {
				err = fmt.Errorf("accent_word must be a word from the headline")
			}
		case "story":
			if s.Title == "" || s.Text == "" {
				return "", fmt.Errorf("sections[%d] story needs title and text", i)
			}
			if err = capStr("title", s.Title, 80); err == nil {
				err = capStr("text", s.Text, 1200)
			}
			// The image-bearing layouts fall through to the text-only branch in
			// front-page.php without an image, so the model would believe it
			// chose a split shape while the visitor sees a centred text well —
			// and validateComposition would count the declared shape, not the
			// rendered one. Same guard cta_band's image-bleed gets.
			if err == nil && s.ImageID <= 0 {
				switch s.Layout {
				case "image-left", "image-right", "offset-frame":
					err = fmt.Errorf("story layout %s needs image_id (without one it renders as text-only) — generate an interior/detail shot or use layout text-only", s.Layout)
				}
			}
		case "cta_band":
			if s.Headline == "" {
				return "", fmt.Errorf("sections[%d] cta_band needs headline", i)
			}
			if err = capStr("headline", s.Headline, 90); err == nil {
				if err = capStr("text", s.Text, 160); err == nil {
					err = capStr("button_label", s.ButtonLabel, 30)
				}
			}
			if err == nil && s.Layout == "image-bleed" && s.ImageID <= 0 {
				err = fmt.Errorf("cta_band layout image-bleed needs image_id (the band is set over a photograph)")
			}
		case "hours_location":
			if len(s.Hours) > 7 {
				return "", fmt.Errorf("sections[%d] hours_location: max 7 hour lines", i)
			}
			for _, h := range s.Hours {
				if err = capStr("hours line", h, 60); err != nil {
					break
				}
			}
			if err == nil {
				if err = capStr("email", s.Email, 120); err != nil {
					return "", err
				}
				if err = capStr("whatsapp", s.WhatsApp, 24); err != nil {
					return "", err
				}
				if err = capStr("instagram", s.Instagram, 80); err != nil {
					return "", err
				}
				if err = capStr("address", s.Address, 200); err == nil {
					err = capStr("phone", s.Phone, 30)
				}
			}
		case "gallery":
			if len(s.ImageIDs) < 3 || len(s.ImageIDs) > 8 {
				return "", fmt.Errorf("sections[%d] gallery needs 3-8 image_ids", i)
			}
			for _, id := range s.ImageIDs {
				if id <= 0 {
					return "", fmt.Errorf("sections[%d] gallery image_ids must be positive attachment IDs", i)
				}
			}
		case "offer_banner":
			if s.Text == "" {
				return "", fmt.Errorf("sections[%d] offer_banner needs text", i)
			}
			err = capStr("text", s.Text, 120)
		case "announcement":
			if len(s.Chips) < 2 || len(s.Chips) > 4 {
				return "", fmt.Errorf("sections[%d] announcement needs 2-4 short chips (trust claims / offers)", i)
			}
			for _, c := range s.Chips {
				if err = capStr("chip", c, 60); err != nil {
					break
				}
			}
		case "testimonials":
			if len(s.Items) < 2 || len(s.Items) > 4 {
				return "", fmt.Errorf("sections[%d] testimonials needs 2-4 items — and ONLY quotes the user actually provided, never invented", i)
			}
			for _, it := range s.Items {
				if it.Quote == "" || it.Name == "" {
					return "", fmt.Errorf("sections[%d] testimonial items need quote and name", i)
				}
				if err = capStr("quote", it.Quote, 280); err != nil {
					break
				}
				if err = capStr("meta", it.Meta, 60); err != nil {
					break
				}
			}
		case "bestsellers":
			err = capStr("title", s.Title, 80)
		}
		if err != nil {
			return "", fmt.Errorf("sections[%d] %s: %w", i, s.Type, err)
		}
	}
	if err := validateComposition(secs); err != nil {
		return "", err
	}
	norm, _ := json.Marshal(secs)
	return string(norm), nil
}

// handleSetHomeSections validates and stores the composition; front-page.php
// renders it on the next request. Idempotent full replace.
func (a *API) handleSetHomeSections(site string, params map[string]string) (string, int, error) {
	norm, err := validateHomeSections(params["sections"])
	if err != nil {
		return "", -1, err
	}
	// Footer chrome reads the store contact site-wide: mirror hours_location.
	var contact struct {
		Hours     []string `json:"hours,omitempty"`
		Address   string   `json:"address,omitempty"`
		Phone     string   `json:"phone,omitempty"`
		Email     string   `json:"email,omitempty"`
		WhatsApp  string   `json:"whatsapp,omitempty"`
		Instagram string   `json:"instagram,omitempty"`
	}
	var secs []homeSection
	_ = json.Unmarshal([]byte(norm), &secs)
	for _, sc := range secs {
		if sc.Type == "hours_location" {
			contact.Hours, contact.Address, contact.Phone = sc.Hours, sc.Address, sc.Phone
			contact.Email, contact.WhatsApp, contact.Instagram = sc.Email, sc.WhatsApp, sc.Instagram
		}
	}
	contactJSON, _ := json.Marshal(contact)
	b64c := base64.StdEncoding.EncodeToString(contactJSON)
	b64 := base64.StdEncoding.EncodeToString([]byte(norm))
	phpCode := `
		update_option('hostplane_home_sections', base64_decode("` + b64 + `"));
		update_option('hostplane_contact', base64_decode("` + b64c + `"));
		$secs = json_decode(get_option('hostplane_home_sections'), true);
		echo 'home sections set: ' . count($secs) . ' section(s) — ';
		foreach ($secs as $s) { echo $s['type'] . ' '; }
	`
	out, exit, xerr := a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(phpCode) + " --allow-root 2>&1"})
	if xerr != nil {
		return "", -1, xerr
	}
	return strings.TrimSpace(out), exit, nil
}
