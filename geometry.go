package main

// Geometry tokens (design-variance v3, §1). Until now every scaffolded store
// shared one physical vocabulary — rounded-xl cards, shadow-sm, max-w-7xl,
// py-16 md:py-24, gap-6 — so a florist and a gadget shop differed only by hex
// code. Geometry is the missing token layer: five orthogonal stances that
// resolve to CSS custom properties, consumed through Tailwind token classes
// (rounded-card, shadow-card, max-w-site, py-section, gap-grid, p-card) exactly
// the way the colour tokens already are.
//
// The payoff is store-wide, not homepage-only: because these land as CSS vars
// in style.css, archive-product.php, single-product.php, cart and checkout
// inherit the stance too — the pages a customer actually spends time on.
//
// The default stance (soft/soft/balanced/standard/none) resolves to the exact
// pre-v3 token VALUES — rounded-xl, shadow-sm, py-16 md:py-24, p-5, gap-6,
// max-w-7xl — so an unspecified geometry is not a redesign. It is not literally
// byte-for-byte across every template: a handful of ad-hoc radii and shadows
// (rounded-2xl/3xl, shadow-xl/2xl) were deliberately folded into the token
// scale during the swap, which is what makes a stance reach the whole store
// instead of only the parts nobody had hand-tuned.

import (
	"fmt"
	"sort"
	"strings"
)

// geometryTokens is the resolved CSS-variable payload. Every field is a raw
// CSS value — the caller only interpolates.
type geometryTokens struct {
	RadiusCard string
	RadiusBtn  string
	RadiusPill string

	ShadowCard string
	ShadowLift string

	SpaceSection   string // desktop section rhythm
	SpaceSectionSm string // mobile section rhythm
	SpaceCard      string
	GapGrid        string

	ContainerMax string

	// Divider is the normalized stance name; front-page.php branches on it
	// (band alternates plain-section backgrounds in PHP, where a counter can
	// do it deterministically) and dividerCSS covers the "rule" case.
	Divider string
}

type radiusStance struct{ card, btn, pill string }

// radiusStances: sharp reads as tailored/editorial, round as friendly/retail.
var radiusStances = map[string]radiusStance{
	"sharp": {card: "0px", btn: "0px", pill: "0.125rem"},
	"soft":  {card: "0.75rem", btn: "0.5rem", pill: "9999px"},
	"round": {card: "1.5rem", btn: "9999px", pill: "9999px"},
}

type elevationStance struct{ card, lift string }

// elevationStances: flat/hairline lean on borders instead of shadow — the
// difference between a luxe boutique and a marketplace.
var elevationStances = map[string]elevationStance{
	"flat":     {card: "none", lift: "none"},
	"hairline": {card: "none", lift: "0 1px 2px rgba(0,0,0,0.06)"},
	"soft":     {card: "0 1px 2px rgba(0,0,0,0.06)", lift: "0 8px 24px -8px rgba(0,0,0,0.18)"},
	"lifted":   {card: "0 4px 12px -2px rgba(0,0,0,0.10)", lift: "0 20px 45px -15px rgba(0,0,0,0.28)"},
}

type densityStance struct{ section, sectionSm, card, gap string }

// densityStances: "balanced" is the pre-v3 rhythm exactly (py-16 md:py-24,
// p-5, gap-6) so the default stance is not a visual change.
var densityStances = map[string]densityStance{
	"airy":     {section: "8rem", sectionSm: "5rem", card: "2rem", gap: "2.5rem"},
	"balanced": {section: "6rem", sectionSm: "4rem", card: "1.25rem", gap: "1.5rem"},
	"dense":    {section: "3.25rem", sectionSm: "2.25rem", card: "1rem", gap: "1rem"},
}

// measureStances: "standard" is max-w-7xl (80rem) — the pre-v3 container.
var measureStances = map[string]string{
	"wide":     "90rem",
	"standard": "80rem",
	"narrow":   "66rem",
}

// dividerStances is a set; the rendering split lives in dividerCSS (rule) and
// front-page.php (band).
var dividerStances = map[string]bool{
	"none": true, "rule": true, "band": true,
}

// geometryDefaults is the pre-v3 look. Applied field-by-field, so a brief may
// specify only the stances it cares about.
const (
	defaultRadius    = "soft"
	defaultElevation = "soft"
	defaultDensity   = "balanced"
	defaultMeasure   = "standard"
	defaultDivider   = "none"
)

// keysOf renders an allowlist for error messages, sorted so the text is stable
// across runs (map iteration order would otherwise churn the test fixtures).
func keysOf[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// resolveGeometry validates the five stances and computes the token payload.
// Pure; unit-tested. Empty values take the pre-v3 default rather than failing —
// geometry is additive, and an old caller must keep working unchanged.
func resolveGeometry(radius, elevation, density, measure, divider string) (geometryTokens, error) {
	var g geometryTokens

	if radius == "" {
		radius = defaultRadius
	}
	r, ok := radiusStances[radius]
	if !ok {
		return g, fmt.Errorf("geometry.radius %q is not a stance — choose one of: %s", radius, keysOf(radiusStances))
	}
	if elevation == "" {
		elevation = defaultElevation
	}
	e, ok := elevationStances[elevation]
	if !ok {
		return g, fmt.Errorf("geometry.elevation %q is not a stance — choose one of: %s", elevation, keysOf(elevationStances))
	}
	if density == "" {
		density = defaultDensity
	}
	d, ok := densityStances[density]
	if !ok {
		return g, fmt.Errorf("geometry.density %q is not a stance — choose one of: %s", density, keysOf(densityStances))
	}
	if measure == "" {
		measure = defaultMeasure
	}
	m, ok := measureStances[measure]
	if !ok {
		return g, fmt.Errorf("geometry.measure %q is not a stance — choose one of: %s", measure, keysOf(measureStances))
	}
	if divider == "" {
		divider = defaultDivider
	}
	if !dividerStances[divider] {
		return g, fmt.Errorf("geometry.divider %q is not a stance — choose one of: %s", divider, keysOf(dividerStances))
	}

	return geometryTokens{
		RadiusCard:     r.card,
		RadiusBtn:      r.btn,
		RadiusPill:     r.pill,
		ShadowCard:     e.card,
		ShadowLift:     e.lift,
		SpaceSection:   d.section,
		SpaceSectionSm: d.sectionSm,
		SpaceCard:      d.card,
		GapGrid:        d.gap,
		ContainerMax:   m,
		Divider:        divider,
	}, nil
}

// cssVars renders the geometry block for style.css's :root.
func (g geometryTokens) cssVars() string {
	return strings.Join([]string{
		"  --radius-card: " + g.RadiusCard + ";",
		"  --radius-btn: " + g.RadiusBtn + ";",
		"  --radius-pill: " + g.RadiusPill + ";",
		"  --shadow-card: " + g.ShadowCard + ";",
		"  --shadow-lift: " + g.ShadowLift + ";",
		"  --space-section: " + g.SpaceSection + ";",
		"  --space-section-sm: " + g.SpaceSectionSm + ";",
		"  --space-card: " + g.SpaceCard + ";",
		"  --gap-grid: " + g.GapGrid + ";",
		"  --container-max: " + g.ContainerMax + ";",
	}, "\n")
}

// dividerCSS is the separation rule baked into style.css. "band" is handled in
// front-page.php instead: alternating a background needs a counter over plain
// sections, and a CSS nth-child would fight the sections that set their own
// background (bg-primary, bg-card) at equal-or-higher specificity.
func (g geometryTokens) dividerCSS() string {
	if g.Divider == "rule" {
		return ".hp-section + .hp-section { border-top: 1px solid var(--color-border); }"
	}
	return ""
}
