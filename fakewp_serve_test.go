package main

// A stand-in control plane for end-to-end runs of the store-build workflow.
//
// It fakes only the part that needs real infrastructure — Docker, WordPress,
// MariaDB — and runs the REAL validators and the REAL theme generator for
// everything else, because those are the code under test. create_theme goes
// through resolveGeometry/resolveThemeFonts/buildThemeScript and writes the
// generated PHP to disk so the finished store can be rendered afterwards;
// set_home_sections goes through validateHomeSections, so a composition the
// live platform would reject is rejected here too.
//
// Opt-in, like the preview emitter:
//
//	FAKEWP_ADDR=:9099 FAKEWP_OUT=/tmp/fakewp go test -run TestServeFakeWPExec -timeout 0
//
// State per site lands in $FAKEWP_OUT/<site>/ as create-theme.php,
// sections.json and state.json — the same shape the preview harness renders.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeProduct struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price string `json:"price"`
}

type fakeSite struct {
	mu         sync.Mutex
	Currency   string            `json:"currency"`
	ThemeSlug  string            `json:"theme_slug"`
	ThemeOK    bool              `json:"theme_activated"`
	Design     map[string]string `json:"design"`
	Sections   string            `json:"sections"`
	Products   []fakeProduct     `json:"products"`
	Categories []string          `json:"categories"`
	Pages      []string          `json:"pages"`
	Options    map[string]string `json:"options"`
	Images     int               `json:"images_generated"`
	Calls      []string          `json:"calls"`
	nextID     int
}

type fakeWP struct {
	mu    sync.Mutex
	sites map[string]*fakeSite
	out   string
}

func (f *fakeWP) site(key string) *fakeSite {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sites[key] == nil {
		f.sites[key] = &fakeSite{Options: map[string]string{}, nextID: 100}
	}
	return f.sites[key]
}

type wpExecReq struct {
	Tool   string            `json:"tool"`
	Params map[string]string `json:"params"`
}

type wpExecResp struct {
	Site     string `json:"site"`
	Tool     string `json:"tool"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

func TestServeFakeWPExec(t *testing.T) {
	addr := os.Getenv("FAKEWP_ADDR")
	if addr == "" {
		t.Skip("set FAKEWP_ADDR to serve the fake control plane")
	}
	out := os.Getenv("FAKEWP_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "fakewp")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeWP{sites: map[string]*fakeSite{}, out: out}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/sites/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/sites/")
		site, action, _ := strings.Cut(rest, "/")
		switch {
		case action == "wp-exec" && r.Method == "POST":
			f.handleExec(t, w, r, site)
		case action == "" && r.Method == "GET":
			writeJSON(w, 200, map[string]any{
				"site": site, "status": "ACTIVE",
				"domain": site + ".cowsaidmoo.tech", "url": "https://" + site + ".cowsaidmoo.tech",
			})
		default:
			writeJSON(w, 404, map[string]any{"error": "not found: " + r.URL.Path})
		}
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	t.Logf("fake control plane listening on %s (state -> %s)", addr, out)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeWP) handleExec(t *testing.T, w http.ResponseWriter, r *http.Request, siteKey string) {
	var req wpExecReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s := f.site(siteKey)
	s.mu.Lock()
	s.Calls = append(s.Calls, req.Tool)
	s.mu.Unlock()

	out, code, err := f.dispatch(siteKey, s, req.Tool, req.Params)
	t.Logf("[%s] %-24s exit=%d %s", siteKey, req.Tool, code, fakeFirstLine(out, err))
	f.persist(siteKey, s)

	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	resp := wpExecResp{Site: siteKey, Tool: req.Tool, Output: out, ExitCode: code}
	if code != 0 {
		writeJSON(w, 422, resp)
		return
	}
	writeJSON(w, 200, resp)
}

func fakeFirstLine(out string, err error) string {
	if err != nil {
		return "ERR " + err.Error()
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	if len(out) > 120 {
		out = out[:120] + "…"
	}
	return out
}

func (f *fakeWP) persist(siteKey string, s *fakeSite) {
	dir := filepath.Join(f.out, siteKey)
	_ = os.MkdirAll(dir, 0o755)
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "state.json"), b, 0o644)
	if s.Sections != "" {
		_ = os.WriteFile(filepath.Join(dir, "sections.json"), []byte(s.Sections), 0o644)
	}
}

func (f *fakeWP) dispatch(siteKey string, s *fakeSite, tool string, p map[string]string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	newID := func() string { s.nextID++; return strconv.Itoa(s.nextID) }

	switch tool {
	case "get_site_overview":
		b, _ := json.Marshal(map[string]any{
			"wordpress": true, "woocommerce": true, "currency": s.Currency,
			"theme": s.ThemeSlug, "products": len(s.Products), "pages": s.Pages,
		})
		return string(b), 0, nil

	case "prepare_store":
		s.Currency = p["currency"]
		if t := p["store_name"]; t != "" {
			s.Options["blogname"] = t
		}
		if t := p["tagline"]; t != "" {
			s.Options["blogdescription"] = t
		}
		s.Pages = append(s.Pages, "Shop", "Cart", "Checkout", "My account")
		return "store prepared: currency " + s.Currency + ", default content removed, permalinks set", 0, nil

	case "create_theme":
		// REAL generator + REAL validators. A bad stance or an uncurated font
		// fails here exactly as it would on the live platform.
		geo, err := resolveGeometry(p["radius"], p["elevation"], p["density"], p["measure"], p["divider"])
		if err != nil {
			return err.Error(), 1, nil
		}
		fd, fb := p["font_display"], p["font_body"]
		if fd == "" {
			fd = "Plus Jakarta Sans"
		}
		if fb == "" {
			fb = "Inter"
		}
		fontsURL, ferr := resolveThemeFonts(fd, fb)
		if ferr != nil {
			return ferr.Error(), 1, nil
		}
		slug := p["slug"]
		if !themeSlugSafeRe.MatchString(slug) {
			return "slug must be lowercase alphanumeric with hyphens", 1, nil
		}
		pr, sc, ac := orFake(p["primary_color"], "#2563eb"), orFake(p["secondary_color"], "#10b981"), orFake(p["accent_color"], "#f59e0b")
		script := buildThemeScript(p["name"], slug, strings.ReplaceAll(slug, "-", "_"),
			p["description"], pr, sc, ac, fd, fb, fontsURL, geo)

		dir := filepath.Join(f.out, siteKey)
		_ = os.MkdirAll(dir, 0o755)
		if werr := os.WriteFile(filepath.Join(dir, "create-theme.php"), []byte(script), 0o644); werr != nil {
			return "", 0, werr
		}
		meta, _ := json.MarshalIndent(map[string]string{
			"business": p["name"], "slug": slug, "divider": geo.Divider,
			"stance": strings.Join([]string{orFake(p["radius"], "soft"), orFake(p["elevation"], "soft"),
				orFake(p["density"], "balanced"), orFake(p["measure"], "standard")}, "/"),
			"type": fd + " / " + fb,
		}, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644)

		s.ThemeSlug = slug
		s.Design = map[string]string{
			"primary": pr, "secondary": sc, "accent": ac, "font_display": fd, "font_body": fb,
			"radius": orFake(p["radius"], "soft"), "elevation": orFake(p["elevation"], "soft"),
			"density": orFake(p["density"], "balanced"), "measure": orFake(p["measure"], "standard"),
			"divider": geo.Divider,
		}
		return fmt.Sprintf("Theme '%s' created at themes/%s with 9 template files + Tailwind CSS design system", p["name"], slug), 0, nil

	case "activate_theme":
		if p["slug"] != "" && p["slug"] != s.ThemeSlug {
			return "theme not found: " + p["slug"], 1, nil
		}
		s.ThemeOK = true
		return "activated theme " + s.ThemeSlug, 0, nil

	case "set_home_sections":
		norm, err := validateHomeSections(p["sections"])
		if err != nil {
			return err.Error(), 1, nil
		}
		s.Sections = norm
		var secs []homeSection
		_ = json.Unmarshal([]byte(norm), &secs)
		var kinds []string
		for _, sc := range secs {
			kinds = append(kinds, sc.Type+"/"+sc.Layout)
		}
		return fmt.Sprintf("home sections set: %d section(s) — %s", len(secs), strings.Join(kinds, " ")), 0, nil

	case "setup_product", "create_product":
		id := newID()
		s.Products = append(s.Products, fakeProduct{ID: id, Name: p["name"], Price: orFake(p["regular_price"], p["price"])})
		return "Success: Created product " + id + ".", 0, nil

	case "create_product_category":
		s.Categories = append(s.Categories, p["name"])
		return "Success: Created product_cat " + newID() + ".", 0, nil

	case "setup_store_page", "create_post":
		title := orFake(p["title"], "Page")
		s.Pages = append(s.Pages, title)
		return "Success: Created page " + newID() + ".", 0, nil

	case "generate_image", "upload_image", "import_media":
		s.Images++
		return "Attachment ID: " + newID(), 0, nil

	case "verify_page":
		return "verify_page " + orFake(p["path"], "/") + ": PASSING\nfinal_status=200 tailwind=true\ntitle=\"" + s.Options["blogname"] + "\"\nchecks_failed=[]", 0, nil

	case "verify_store":
		return f.verify(s, p)

	case "media_pass_state":
		b, _ := json.Marshal(map[string]any{"sections": []any{}, "site_icon": "1", "products": []any{}, "placeholders": []string{}})
		return string(b), 0, nil
	}

	// Everything else is a write the workflow does not assert on.
	return "ok (" + tool + ")", 0, nil
}

func orFake(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// verify mirrors the shape of the platform's acceptance check closely enough
// that the agent's repair loop behaves the same way.
func (f *fakeWP) verify(s *fakeSite, p map[string]string) (string, int, error) {
	var fails []string
	if s.ThemeSlug == "" || !s.ThemeOK {
		fails = append(fails, "theme_not_active")
	}
	if want := p["currency"]; want != "" && want != s.Currency {
		fails = append(fails, fmt.Sprintf("currency_mismatch: store %q, expected %q", s.Currency, want))
	}
	if raw := p["products"]; raw != "" {
		var want []struct{ Name, Price string }
		if json.Unmarshal([]byte(raw), &want) == nil {
			for _, wp := range want {
				found := false
				for _, hp := range s.Products {
					if strings.EqualFold(strings.TrimSpace(hp.Name), strings.TrimSpace(wp.Name)) {
						found = true
						break
					}
				}
				if !found {
					fails = append(fails, "missing_product: "+wp.Name)
				}
			}
		}
	}
	var secs []homeSection
	_ = json.Unmarshal([]byte(s.Sections), &secs)
	if len(secs) < 4 {
		fails = append(fails, fmt.Sprintf("homepage_too_thin: %d typed sections (need >=4 via set_home_sections)", len(secs)))
	}
	if len(fails) > 0 {
		return "verify_store: FAILING\nchecks_failed=" + strings.Join(fails, "; "), 1, nil
	}
	return fmt.Sprintf("verify_store: PASSING\ncurrency=%s products=%d sections=%d theme=%s",
		s.Currency, len(s.Products), len(secs), s.ThemeSlug), 0, nil
}
