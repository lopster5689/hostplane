package main

import (
	"regexp"
	"strings"
	"testing"
)

// mustGeometry is the test helper for callers that just need a valid payload.
func mustGeometry(t *testing.T, radius, elevation, density, measure, divider string) geometryTokens {
	t.Helper()
	g, err := resolveGeometry(radius, elevation, density, measure, divider)
	if err != nil {
		t.Fatalf("resolveGeometry(%q,%q,%q,%q,%q): %v", radius, elevation, density, measure, divider, err)
	}
	return g
}

// The default stance must reproduce the pre-v3 look exactly. If this test
// fails, every existing store's geometry silently moved.
func TestGeometryDefaultsMatchPreV3Look(t *testing.T) {
	g := mustGeometry(t, "", "", "", "", "")
	for _, c := range []struct{ got, want, what string }{
		{g.RadiusCard, "0.75rem", "card radius (was rounded-xl)"},
		{g.RadiusBtn, "0.5rem", "button radius (was rounded-lg)"},
		{g.RadiusPill, "9999px", "pill radius (was rounded-full)"},
		{g.ShadowCard, "0 1px 2px rgba(0,0,0,0.06)", "card shadow (was shadow-sm)"},
		{g.SpaceSection, "6rem", "desktop rhythm (was md:py-24)"},
		{g.SpaceSectionSm, "4rem", "mobile rhythm (was py-16)"},
		{g.SpaceCard, "1.25rem", "card padding (was p-5)"},
		{g.GapGrid, "1.5rem", "grid gap (was gap-6)"},
		{g.ContainerMax, "80rem", "container (was max-w-7xl)"},
		{g.Divider, "none", "divider"},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.what, c.got, c.want)
		}
	}
}

func TestGeometryRejectsUnknownStances(t *testing.T) {
	for _, c := range []struct{ r, e, d, m, dv, field string }{
		{"squircle", "", "", "", "", "radius"},
		{"", "floating", "", "", "", "elevation"},
		{"", "", "roomy", "", "", "density"},
		{"", "", "", "gigantic", "", "measure"},
		{"", "", "", "", "stripes", "divider"},
	} {
		_, err := resolveGeometry(c.r, c.e, c.d, c.m, c.dv)
		if err == nil {
			t.Errorf("%s: unknown stance accepted", c.field)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: error should name the field, got %q", c.field, err)
		}
	}
}

// Every stance must produce a DIFFERENT payload — a stance that resolves to the
// same values as another is decoration, not variance.
func TestGeometryStancesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, r := range []string{"sharp", "soft", "round"} {
		g := mustGeometry(t, r, "", "", "", "")
		key := g.RadiusCard + "|" + g.RadiusBtn + "|" + g.RadiusPill
		if prev, dup := seen[key]; dup {
			t.Errorf("radius %q resolves identically to %q", r, prev)
		}
		seen[key] = r
	}
	seen = map[string]string{}
	for _, d := range []string{"airy", "balanced", "dense"} {
		g := mustGeometry(t, "", "", d, "", "")
		key := g.SpaceSection + "|" + g.SpaceCard + "|" + g.GapGrid
		if prev, dup := seen[key]; dup {
			t.Errorf("density %q resolves identically to %q", d, prev)
		}
		seen[key] = d
	}
}

func TestGeometryCSSVarsAndDivider(t *testing.T) {
	g := mustGeometry(t, "sharp", "flat", "airy", "narrow", "rule")
	css := g.cssVars()
	for _, want := range []string{
		"--radius-card: 0px;", "--shadow-card: none;",
		"--space-section: 8rem;", "--container-max: 66rem;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("cssVars missing %q\ngot:\n%s", want, css)
		}
	}
	if !strings.Contains(g.dividerCSS(), ".hp-section + .hp-section") {
		t.Errorf("rule divider must emit an adjacent-sibling border, got %q", g.dividerCSS())
	}
	// band is rendered in PHP (a CSS nth-child would fight bg-primary), so it
	// must NOT also emit a stylesheet rule.
	if band := mustGeometry(t, "", "", "", "", "band"); band.dividerCSS() != "" {
		t.Errorf("band divider must not emit CSS, got %q", band.dividerCSS())
	}
}

// The whole point of geometry is that it reaches the generated theme. This
// asserts the tokens land in style.css AND are wired through the Tailwind
// config, so templates can say rounded-card / py-section / max-w-site.
func TestScaffoldEmitsGeometryTokens(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#9b5b32", "#4c4037", "#8a5a2b",
		"Fraunces", "Karla", "https://fonts.example/css",
		mustGeometry(t, "sharp", "flat", "airy", "narrow", "rule"))

	for _, want := range []string{
		"--radius-card: 0px;",
		"--container-max: 66rem;",
		"--space-section: 8rem;",
		".hp-section + .hp-section",
		"card: 'var(--radius-card)'",
		"btn: 'var(--radius-btn)'",
		"card: 'var(--shadow-card)'",
		"site: 'var(--container-max)'",
		"section: 'var(--space-section)'",
		"grid: 'var(--gap-grid)'",
		"update_option('hostplane_geometry_divider', 'rule')",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scaffold missing geometry wiring: %q", want)
		}
	}
}

// hardcodedGeometryRe catches the utilities geometry replaced. A template that
// keeps using them pins that store to the pre-v3 look no matter what stance the
// brand kit chose — the exact bug this feature exists to remove.
var hardcodedGeometryRe = regexp.MustCompile(
	`\b(?:rounded-(?:sm|md|lg|xl|2xl|3xl|full)|shadow-(?:sm|md|lg|xl|2xl)|max-w-7xl|gap-6)\b`)

func TestScaffoldUsesGeometryTokensNotFixedUtilities(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#111111", "#222222", "#333333",
		"Plus Jakarta Sans", "Inter", "https://fonts.example/css",
		mustGeometry(t, "", "", "", "", ""))
	if hits := hardcodedGeometryRe.FindAllString(script, -1); len(hits) > 0 {
		t.Errorf("scaffold hardcodes geometry utilities (use rounded-card/shadow-card/max-w-site/py-section/gap-grid): %v", hits)
	}
}

// Every layout the control plane advertises must actually be rendered by
// front-page.php. A layout in the allowlist with no branch would validate,
// persist, and then silently fall through to the default — variance the model
// believes it chose and the visitor never sees.
func TestEveryAdvertisedLayoutIsRendered(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#111111", "#222222", "#333333",
		"Plus Jakarta Sans", "Inter", "https://fonts.example/css",
		mustGeometry(t, "", "", "", "", ""))
	for typ, layouts := range sectionLayouts {
		// The first entry is the default and is rendered by the else branch,
		// so it needs no literal of its own.
		for _, l := range layouts[1:] {
			if !strings.Contains(script, "'"+l+"'") {
				t.Errorf("%s layout %q is offered but front-page.php has no branch for it", typ, l)
			}
		}
	}
}
