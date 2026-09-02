package main

// Dev utility, not a unit test: emits the REAL create_theme payload for a set
// of contrasting geometry stances so the generated theme can be rendered and
// looked at. Skipped unless PREVIEW_OUT is set.
//
//	PREVIEW_OUT=/tmp/preview go test -run TestEmitPreviewArtifacts ./...
//
// It goes through buildThemeScript and validateHomeSections exactly as a live
// build does, so what gets rendered is what a customer would get — not a
// mock-up of it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type previewVariant struct {
	Key       string
	Business  string
	Primary   string
	Secondary string
	Accent    string
	Display   string
	Body      string
	Radius    string
	Elevation string
	Density   string
	Measure   string
	Divider   string
	Sections  string
}

// Three businesses that should have nothing physical in common. Same feature,
// same scaffold, same tooling — if these render alike, the change failed.
var previewVariants = []previewVariant{
	{
		Key: "jeweller", Business: "Ember & Ash — fine jewellery",
		Primary: "#4a2545", Secondary: "#2f2a3d", Accent: "#8a6a3b",
		Display: "Cormorant Garamond", Body: "Karla",
		Radius: "sharp", Elevation: "flat", Density: "airy", Measure: "narrow", Divider: "rule",
		Sections: `[
		 {"type":"hero_image","image_id":1,"layout":"stacked-editorial","eyebrow":"HAND-SET IN LONDON","headline":"Stones cut in Jaipur, set on Hatton Garden","accent_word":"Jaipur","sub":"Every setting is finished by one bench jeweller, start to end.","chips":["Recycled 18ct gold","Lifetime resizing"]},
		 {"type":"story","image_id":2,"layout":"offset-frame","title":"One bench, forty years","text":"Ash learned stone-setting from his father in 1984 and still works the same bench. We take twelve commissions a month, no more, because that is what two pairs of hands can finish properly."},
		 {"type":"bestsellers","layout":"grid-3-large","title":"Current pieces"},
		 {"type":"gallery","layout":"strip","image_ids":[3,4,5,6]},
		 {"type":"cta_band","layout":"split-rule","headline":"Commissions open for March","text":"Twelve a month. Book a bench appointment.","button_label":"Book a fitting"},
		 {"type":"hours_location","layout":"map-side","hours":["Tue-Fri 10-6","Sat 10-4","Closed Sun-Mon"],"address":"14 Hatton Garden, London EC1N 8AT","phone":"+44 20 7405 1122"}
		]`,
	},
	{
		Key: "market", Business: "Sunder Bazaar — neighbourhood grocery",
		Primary: "#a8321f", Secondary: "#2f6135", Accent: "#b8761c",
		Display: "Outfit", Body: "Inter",
		Radius: "soft", Elevation: "soft", Density: "dense", Measure: "wide", Divider: "band",
		Sections: `[
		 {"type":"announcement","chips":["Free delivery over 500","Open 7am daily","Cash and UPI"]},
		 {"type":"hero_image","image_id":1,"layout":"split","eyebrow":"KORAMANGALA, SINCE 1998","headline":"Vegetables in by five, on the shelf by seven","accent_word":"five","sub":"We buy at the Kalasipalya market every morning and sell what came in that day.","chips":["Same-day produce","Delivery in 2 hours","No minimum order"]},
		 {"type":"category_strip","layout":"tiles"},
		 {"type":"bestsellers","layout":"grid-4","title":"Moving today"},
		 {"type":"story","image_id":2,"layout":"image-right","title":"Third shop, same family","text":"Sunder started with a handcart on 5th Block in 1998. His daughter runs the tills now and still argues with the same tomato supplier."},
		 {"type":"cta_band","layout":"solid","headline":"Order by 8pm for tomorrow morning","text":"We deliver 7am to 9pm across Koramangala.","button_label":"Start an order"},
		 {"type":"hours_location","layout":"inline-bar","hours":["7am-9pm every day"],"address":"221 5th Block, Koramangala, Bengaluru","phone":"+91 80 4112 8890"}
		]`,
	},
	{
		Key: "bakery", Business: "Cross Road Bakehouse — wood-fired bakery",
		Primary: "#9b5b32", Secondary: "#4c4037", Accent: "#8a5a2b",
		Display: "Fraunces", Body: "Karla",
		Radius: "round", Elevation: "lifted", Density: "balanced", Measure: "standard", Divider: "none",
		Sections: `[
		 {"type":"hero_image","image_id":1,"layout":"full-bleed-quiet","eyebrow":"WOOD-FIRED, BENGALURU","headline":"Ragi and jaggery, ground the morning we bake","accent_word":"jaggery","sub":"Two ovens, one baker, bread out by six.","chips":["Out by 6am","Ragi from Mandya"]},
		 {"type":"bestsellers","layout":"asymmetric-feature","title":"Out of the oven"},
		 {"type":"story","image_id":2,"layout":"image-left","title":"Two ovens on Cross Road","text":"We started in 2019 with one oven and a Mandya ragi supplier who still delivers on Tuesdays. Sixty loaves a day, and we stop when they are gone."},
		 {"type":"gallery","layout":"masonry","image_ids":[3,4,5,6,7]},
		 {"type":"cta_band","layout":"image-bleed","image_id":8,"headline":"Order fresh for tomorrow","text":"Cut-off is 8pm.","button_label":"Order now"},
		 {"type":"hours_location","layout":"card","hours":["Tue-Sun 6am-2pm","Closed Monday"],"address":"9 Cross Road, Bengaluru 560095","phone":"+91 80 2200 1188"}
		]`,
	},
}

func TestEmitPreviewArtifacts(t *testing.T) {
	out := os.Getenv("PREVIEW_OUT")
	if out == "" {
		t.Skip("set PREVIEW_OUT to emit preview artifacts")
	}
	for _, v := range previewVariants {
		dir := filepath.Join(out, v.Key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Same validation a live set_home_sections call runs — if a composition
		// here is rejected, the preview is telling us something real.
		sections, err := validateHomeSections(v.Sections)
		if err != nil {
			t.Fatalf("%s: composition rejected: %v", v.Key, err)
		}

		geo, err := resolveGeometry(v.Radius, v.Elevation, v.Density, v.Measure, v.Divider)
		if err != nil {
			t.Fatalf("%s: %v", v.Key, err)
		}
		fontsURL, err := resolveThemeFonts(v.Display, v.Body)
		if err != nil {
			t.Fatalf("%s: %v", v.Key, err)
		}

		script := buildThemeScript(v.Business, v.Key, v.Key, v.Business,
			v.Primary, v.Secondary, v.Accent, v.Display, v.Body, fontsURL, geo)

		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("create-theme.php", script)
		write("sections.json", sections)
		meta, _ := json.MarshalIndent(map[string]string{
			"business": v.Business, "slug": v.Key, "divider": geo.Divider,
			"stance": v.Radius + "/" + v.Elevation + "/" + v.Density + "/" + v.Measure,
			"type":   v.Display + " / " + v.Body,
		}, "", "  ")
		write("meta.json", string(meta))
		t.Logf("emitted %s (%s)", v.Key, dir)
	}
}
