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
			if err == nil && s.Layout != "" && s.Layout != "full" && s.Layout != "split" {
				err = fmt.Errorf("layout must be full or split")
			}
		case "story":
			if s.Title == "" || s.Text == "" {
				return "", fmt.Errorf("sections[%d] story needs title and text", i)
			}
			if err = capStr("title", s.Title, 80); err == nil {
				err = capStr("text", s.Text, 1200)
			}
			// optional image_id -> two-column editorial story
		case "cta_band":
			if s.Headline == "" {
				return "", fmt.Errorf("sections[%d] cta_band needs headline", i)
			}
			if err = capStr("headline", s.Headline, 90); err == nil {
				if err = capStr("text", s.Text, 160); err == nil {
					err = capStr("button_label", s.ButtonLabel, 30)
				}
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
