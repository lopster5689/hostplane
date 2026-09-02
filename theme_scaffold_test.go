package main

import (
	"regexp"
	"strings"
	"testing"
)

// bannedPaletteClassRe: raw Tailwind palette classes that would bypass the
// token system. The scaffold must express EVERY color through token classes
// (bg-primary, text-ink, border-line, ...) so a create_theme palette change
// restyles the whole store — the "blue store forever" bug class (design-depth
// plan D1). white/black literals are allowed (footer alpha text, badges).
var bannedPaletteClassRe = regexp.MustCompile(
	`(?:bg|text|border|from|to|ring|shadow|fill|stroke)-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d+`)

func TestScaffoldHasNoHardcodedPaletteClasses(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#111111", "#222222", "#333333",
		"Plus Jakarta Sans", "Inter", "https://fonts.example/css", mustGeometry(t, "", "", "", "", ""))
	if hits := bannedPaletteClassRe.FindAllString(script, -1); len(hits) > 0 {
		t.Fatalf("scaffold contains raw palette classes (must use token classes): %v", hits)
	}
}

func TestScaffoldWiresTokensThroughTailwindConfig(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#9b5b32", "#4c4037", "#f1e3cf",
		"Playfair Display", "Karla", "https://fonts.example/css", mustGeometry(t, "", "", "", "", ""))
	for _, want := range []string{
		"primary: 'var(--color-primary)'",
		"'primary-hover': 'var(--color-primary-hover)'",
		"ink: 'var(--color-text)'",
		"line: 'var(--color-border)'",
		"footer: 'var(--color-footer)'",
		"--color-primary-hover: color-mix(in oklab, $primary, black 10%)",
		"bg-primary", "text-ink", "border-line", "bg-footer",
		"$font_display = 'Playfair Display'",
		"$display_fallback = 'Georgia, serif'", // serif face gets a serif stack
		"str_replace('__FONT_DISPLAY__', $font_display",
		"str_replace('__FONTS_URL__', $fonts_url",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scaffold missing token wiring: %q", want)
		}
	}
}

func TestResolveThemeFonts(t *testing.T) {
	url, err := resolveThemeFonts("Fraunces", "Karla")
	if err != nil || !strings.Contains(url, "Fraunces") || !strings.Contains(url, "Karla") {
		t.Errorf("valid pairing rejected: %v %q", err, url)
	}
	if _, err := resolveThemeFonts("Comic Sans MS", "Inter"); err == nil {
		t.Error("unknown display face must be rejected (it would 404 silently)")
	}
	if _, err := resolveThemeFonts("Fraunces", "Wingdings"); err == nil {
		t.Error("unknown body face must be rejected")
	}
	if fontFallback("Fraunces") != "Georgia, serif" || fontFallback("Sora") != "system-ui, sans-serif" {
		t.Error("fallback stacks wrong")
	}
}

// The Tailwind config is what every heading actually resolves through, so its
// fallback stack must match style.css's --font-heading. When they disagreed, a
// serif brand fell back to generic sans-serif on any font-load failure — the
// brand decision silently inverted.
func TestFontFallbackStacksAgreeBetweenCSSAndTailwind(t *testing.T) {
	serif := buildThemeScript("T", "t", "t", "d", "#9b5b32", "#4c4037", "#8a5a2b",
		"Fraunces", "Karla", "https://fonts.example/css", mustGeometry(t, "", "", "", "", ""))
	if !strings.Contains(serif, `heading: ['__FONT_DISPLAY__', __DISPLAY_FALLBACK__]`) {
		t.Error("tailwind heading fallback is not templated from the computed stack")
	}
	if !strings.Contains(serif, `str_replace('__DISPLAY_FALLBACK__'`) {
		t.Error("display fallback is never substituted")
	}
	// A serif display face must carry the serif stack into BOTH channels.
	if !strings.Contains(serif, `$display_fallback = 'Georgia, serif'`) {
		t.Error("serif face did not get the serif fallback stack")
	}

	sans := buildThemeScript("T", "t", "t", "d", "#9b5b32", "#4c4037", "#8a5a2b",
		"Sora", "Inter", "https://fonts.example/css", mustGeometry(t, "", "", "", "", ""))
	if !strings.Contains(sans, `$display_fallback = 'system-ui, sans-serif'`) {
		t.Error("sans face did not get the sans fallback stack")
	}
}
