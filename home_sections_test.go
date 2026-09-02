package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestLayoutAllowlist(t *testing.T) {
	ok := `[{"type":"bestsellers","layout":"carousel-row"}]`
	if _, err := validateHomeSections(ok); err != nil {
		t.Errorf("advertised layout rejected: %v", err)
	}
	bad := `[{"type":"bestsellers","layout":"waterfall"}]`
	err := errOf(validateHomeSections(bad))
	if err == nil {
		t.Fatal("unknown layout accepted")
	}
	// The message has to list the alternatives or the model cannot self-correct.
	if !strings.Contains(err.Error(), "carousel-row") {
		t.Errorf("rejection should name the available layouts, got %q", err)
	}
	// A type with nothing to vary must reject a layout rather than ignore it.
	if errOf(validateHomeSections(`[{"type":"announcement","chips":["a","b"],"layout":"fancy"}]`)) == nil {
		t.Error("announcement takes no layout but one was accepted")
	}
}

// An omitted layout must resolve to the pre-v3 rendering, and story is the one
// type whose default depends on whether it has an image.
func TestLayoutDefaultsPreservePreV3Rendering(t *testing.T) {
	for _, c := range []struct{ in, wantLayout string }{
		{`[{"type":"hero_image","image_id":1,"headline":"h"}]`, "full"},
		{`[{"type":"bestsellers"}]`, "grid-4"},
		{`[{"type":"story","title":"t","text":"x"}]`, "text-only"},
		{`[{"type":"story","title":"t","text":"x","image_id":9}]`, "image-left"},
	} {
		norm, err := validateHomeSections(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		var secs []homeSection
		if err := json.Unmarshal([]byte(norm), &secs); err != nil {
			t.Fatal(err)
		}
		if secs[0].Layout != c.wantLayout {
			t.Errorf("%s: default layout = %q, want %q", c.in, secs[0].Layout, c.wantLayout)
		}
	}
}

func TestCompositionConstraints(t *testing.T) {
	hero := `{"type":"hero_image","image_id":1,"headline":"Ragi and jaggery"}`
	story := `{"type":"story","title":"t","text":"x"}`
	best := `{"type":"bestsellers"}`
	cta := `{"type":"cta_band","headline":"Order for tomorrow"}`
	hours := `{"type":"hours_location","address":"12 MG Road"}`
	gal := `{"type":"gallery","image_ids":[1,2,3]}`
	ann := `{"type":"announcement","chips":["Free delivery","Baked daily"]}`

	// A storefront-sized composition that satisfies every rule.
	good := "[" + strings.Join([]string{hero, story, best, gal, cta, hours}, ",") + "]"
	if _, err := validateHomeSections(good); err != nil {
		t.Fatalf("valid storefront rejected: %v", err)
	}
	// announcement is allowed to precede the hero — the header lifts it out.
	withAnn := "[" + strings.Join([]string{ann, hero, story, best, gal, cta, hours}, ",") + "]"
	if _, err := validateHomeSections(withAnn); err != nil {
		t.Errorf("announcement before hero rejected: %v", err)
	}

	for _, c := range []struct{ name, in, want string }{
		{"no hero first", "[" + strings.Join([]string{story, hero, best, gal, cta, hours}, ",") + "]", "open with hero_image"},
		{"no product surface", "[" + strings.Join([]string{hero, story, gal, cta, hours, story}, ",") + "]", "something for sale"},
		{"no closing cta", "[" + strings.Join([]string{hero, story, best, gal, hours, story}, ",") + "]", "cta_band"},
	} {
		err := errOf(validateHomeSections(c.in))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q should mention %q", c.name, err, c.want)
		}
	}

	// Three consecutive sections of one shape is the templated tell. Three
	// "stack" sections in a row: stacked-editorial hero, text-only story,
	// single-large testimonials.
	quotes := `{"type":"testimonials","layout":"single-large","items":[{"quote":"q1","name":"A"},{"quote":"q2","name":"B"}]}`
	monotone := "[" + strings.Join([]string{
		`{"type":"hero_image","image_id":1,"headline":"h","layout":"stacked-editorial"}`,
		`{"type":"story","title":"t","text":"x","layout":"text-only"}`,
		quotes, best, cta, hours,
	}, ",") + "]"
	err := errOf(validateHomeSections(monotone))
	if err == nil {
		t.Fatal("three stack sections in a row accepted")
	}
	if !strings.Contains(err.Error(), "in a row") {
		t.Errorf("monotony error should explain the run, got %q", err)
	}

	// Breaking the run with a different shape must clear it.
	varied := "[" + strings.Join([]string{
		`{"type":"hero_image","image_id":1,"headline":"h","layout":"stacked-editorial"}`,
		`{"type":"story","title":"t","text":"x","image_id":4,"layout":"image-right"}`,
		quotes, best, cta, hours,
	}, ",") + "]"
	if _, err := validateHomeSections(varied); err != nil {
		t.Errorf("varied composition rejected: %v", err)
	}
}

// cta_band's image-bleed layout renders text over a photograph; without one it
// would render an empty dark band.
func TestCTABandImageBleedNeedsImage(t *testing.T) {
	if errOf(validateHomeSections(`[{"type":"cta_band","headline":"h","layout":"image-bleed"}]`)) == nil {
		t.Error("image-bleed without image_id accepted")
	}
	if _, err := validateHomeSections(`[{"type":"cta_band","headline":"h","layout":"image-bleed","image_id":7}]`); err != nil {
		t.Errorf("image-bleed with image rejected: %v", err)
	}
}

// story's image-bearing layouts fall through to text-only without an image, so
// the declared shape and the rendered shape diverge — which also corrupts the
// monotony rule, since it counts what was declared.
func TestStoryImageLayoutsNeedImage(t *testing.T) {
	for _, l := range []string{"image-left", "image-right", "offset-frame"} {
		in := `[{"type":"story","title":"t","text":"x","layout":"` + l + `"}]`
		err := errOf(validateHomeSections(in))
		if err == nil {
			t.Errorf("%s without image_id accepted — it would silently render as text-only", l)
			continue
		}
		if !strings.Contains(err.Error(), "image_id") {
			t.Errorf("%s: error should name the missing field, got %q", l, err)
		}
		withImg := `[{"type":"story","title":"t","text":"x","image_id":5,"layout":"` + l + `"}]`
		if _, err := validateHomeSections(withImg); err != nil {
			t.Errorf("%s with image_id rejected: %v", l, err)
		}
	}
	// text-only must still be fine without one.
	if _, err := validateHomeSections(`[{"type":"story","title":"t","text":"x","layout":"text-only"}]`); err != nil {
		t.Errorf("text-only story rejected: %v", err)
	}
}

// A (type, layout) pair missing from layoutFamily silently opts that layout out
// of the monotony rule — the rule would then pass a page it should catch.
func TestEveryLayoutHasAFamily(t *testing.T) {
	for typ, layouts := range sectionLayouts {
		for _, l := range layouts {
			if layoutFamily[typ+"/"+l] == "" {
				t.Errorf("%s/%s has no layoutFamily entry — it is invisible to the monotony rule", typ, l)
			}
		}
	}
	// And no family entry may name a layout that does not exist (a typo there
	// is equally silent).
	for key := range layoutFamily {
		typ, l, _ := strings.Cut(key, "/")
		if l == "" {
			continue // announcement/ and offer_banner/ are layout-less
		}
		found := false
		for _, a := range sectionLayouts[typ] {
			if a == l {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("layoutFamily has %q but %s offers no such layout", key, typ)
		}
	}
}

// ── generated-template structure ────────────────────────────────
// There is no PHP binary in this toolchain, so the alternative-syntax control
// flow of the generated front-page.php is checked directly. An unbalanced
// if/endif here is a fatal parse error at create_theme time on a live site.

var (
	parenRe    = `(?:[^()]|\((?:[^()]|\([^()]*\))*\))*`
	phpTokRe   = regexp.MustCompile(`\bif\s*\(` + parenRe + `\)\s*:|\belseif\s*\(` + parenRe + `\)\s*:|\belse\s*:|\bendif\b|\bforeach\s*\(` + parenRe + `\)\s*:|\bendforeach\b`)
	phpBlockRe = regexp.MustCompile(`(?s)<\?php(.*?)(?:\?>|$)`)
)

func extractHeredoc(t *testing.T, script, name string) string {
	t.Helper()
	open := "<<<'" + name + "'\n"
	i := strings.Index(script, open)
	if i < 0 {
		t.Fatalf("heredoc %s not found in generated script", name)
	}
	i += len(open)
	j := strings.Index(script[i:], "\n"+name+"\n")
	if j < 0 {
		t.Fatalf("heredoc %s not terminated", name)
	}
	return script[i : i+j]
}

func TestFrontPageTemplateControlFlowIsBalanced(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#111111", "#222222", "#333333",
		"Plus Jakarta Sans", "Inter", "https://fonts.example/css",
		mustGeometry(t, "", "", "", "", ""))
	tpl := extractHeredoc(t, script, "FRONTPAGE")

	type frame struct {
		kind string
		n    int
	}
	var stack []frame
	for _, blk := range phpBlockRe.FindAllStringSubmatch(tpl, -1) {
		for _, tok := range phpTokRe.FindAllString(blk[1], -1) {
			switch {
			case strings.HasPrefix(tok, "elseif"):
				if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
					t.Fatalf("elseif outside an if: %q", tok)
				}
			case strings.HasPrefix(tok, "if"):
				stack = append(stack, frame{"if", len(stack)})
			case strings.HasPrefix(tok, "foreach"):
				stack = append(stack, frame{"foreach", len(stack)})
			case strings.HasPrefix(tok, "else"):
				if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
					t.Fatalf("else outside an if")
				}
			case tok == "endif":
				if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
					t.Fatalf("endif does not close an if (top=%v)", stack)
				}
				stack = stack[:len(stack)-1]
			case tok == "endforeach":
				if len(stack) == 0 || stack[len(stack)-1].kind != "foreach" {
					t.Fatalf("endforeach does not close a foreach (top=%v)", stack)
				}
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(stack) != 0 {
		t.Fatalf("front-page.php has %d unclosed block(s): %v", len(stack), stack)
	}
}

// The template must escape everything it echoes from stored section content.
func TestFrontPageTemplateEscapesSectionContent(t *testing.T) {
	script := buildThemeScript("T", "t", "t", "d", "#111111", "#222222", "#333333",
		"Plus Jakarta Sans", "Inter", "https://fonts.example/css",
		mustGeometry(t, "", "", "", "", ""))
	tpl := extractHeredoc(t, script, "FRONTPAGE")

	// Any echo of a raw $hp_s[...] value without an escaper is an XSS hole:
	// section content is model-authored and reaches the public homepage.
	raw := regexp.MustCompile(`echo\s+\$hp_s\[`)
	if hits := raw.FindAllString(tpl, -1); len(hits) > 0 {
		t.Errorf("front-page.php echoes unescaped section content %d time(s)", len(hits))
	}
}

func errOf(_ string, err error) error { return err }
