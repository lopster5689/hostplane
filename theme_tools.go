package main

// Harness-v2 theme tooling (AI-EDITOR-V2-DESIGN §3.4, research RT4).
//
//   - edit_theme_file: unique-match search/replace — the LLM edits files instead
//     of regenerating them (output tokens are the expensive direction).
//   - php -l gate: PHP written by the model is linted in /tmp BEFORE it can
//     touch the active theme; a fatal typo can no longer white-screen a site.
//   - verify_page: fetches a page through the site's own nginx and reports
//     status/title/h1/tailwind-present — the "don't claim success unverified"
//     primitive.
//   - activation guard: activate_theme refuses to switch to a custom theme that
//     is missing required templates (the 2026-04 audit's worst failure).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
)

// writeContainerFile writes data to path inside the site's PHP container via
// exec stdin (same pattern as image import — payloads never ride command args).
func (a *API) writeContainerFile(site, path string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	execResp, err := a.docker.ContainerExecCreate(ctx, PHPContainerName(site), types.ExecConfig{
		Cmd:          []string{"sh", "-c", "cat > " + path},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create (write): %w", err)
	}
	attach, err := a.docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return fmt.Errorf("exec attach (write): %w", err)
	}
	_, err = io.Copy(attach.Conn, bytes.NewReader(data))
	attach.CloseWrite()
	attach.Close()
	if err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	for i := 0; i < 20; i++ {
		inspect, ierr := a.docker.ContainerExecInspect(ctx, execResp.ID)
		if ierr != nil {
			return fmt.Errorf("exec inspect (write): %w", ierr)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("container write exited %d", inspect.ExitCode)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("container write timed out")
}

// appendContainerFile appends data to a path inside the site's PHP container
// (chunked-upload support; same stdin pattern as writeContainerFile).
func (a *API) appendContainerFile(site, path string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	execResp, err := a.docker.ContainerExecCreate(ctx, PHPContainerName(site), types.ExecConfig{
		Cmd:          []string{"sh", "-c", "cat >> " + path},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create (append): %w", err)
	}
	attach, err := a.docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return fmt.Errorf("exec attach (append): %w", err)
	}
	_, err = io.Copy(attach.Conn, bytes.NewReader(data))
	attach.CloseWrite()
	attach.Close()
	if err != nil {
		return fmt.Errorf("append payload: %w", err)
	}
	for i := 0; i < 20; i++ {
		inspect, ierr := a.docker.ContainerExecInspect(ctx, execResp.ID)
		if ierr != nil {
			return fmt.Errorf("exec inspect (append): %w", ierr)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("container append exited %d", inspect.ExitCode)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("container append timed out")
}

// phpLint runs `php -l` on a path inside the container. Empty string = clean.
func (a *API) phpLint(site, path string) (string, error) {
	out, exit, err := a.execWPCLI(site, []string{"sh", "-c", "php -l " + path + " 2>&1"})
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return out, nil
	}
	return "", nil
}

// installThemeFile moves a pre-written /tmp payload into the active theme.
func (a *API) installThemeFile(site, tmpPath, filename string) (string, int, error) {
	phpCode := `
		$theme_dir = get_stylesheet_directory();
		$filename = "` + filename + `";
		$path = $theme_dir . "/" . $filename;
		$dir = dirname($path);
		if (!is_dir($dir)) { mkdir($dir, 0755, true); }
		$content = file_get_contents("` + tmpPath + `");
		if ($content === false) { echo "staging file missing"; exit(1); }
		file_put_contents($path, $content);
		unlink("` + tmpPath + `");
		echo "Written " . strlen($content) . " bytes to " . $filename;
	`
	return a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(phpCode) + " --allow-root 2>&1"})
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// handleWriteThemeFileGated is write_theme_file with the php -l gate: stage in
// /tmp, lint (PHP files only), then install.
func (a *API) handleWriteThemeFileGated(site string, params map[string]string) (string, int, error) {
	filename := params["filename"]
	content := params["content"]
	if filename == "" || content == "" {
		return "", -1, fmt.Errorf("filename and content are required")
	}
	if !themeFileAllowlist[filename] {
		return "", -1, fmt.Errorf("filename %q is not allowed", filename)
	}
	if len(content) > 65536 {
		return "", -1, fmt.Errorf("content too large (max 64KB)")
	}

	// Shrink guard (design-depth D9, open-design stub-guard pattern): a
	// "small edit" that replaces a rich template with a stub destroys work.
	// Whole-file writes that shrink an existing file >60% need explicit intent.
	if params["allow_shrink"] != "true" {
		sizeOut, _, serr := a.execWPCLI(site, []string{"sh", "-c",
			"wp eval " + shellQuote(`$p = get_stylesheet_directory() . "/`+filename+`"; echo file_exists($p) ? filesize($p) : 0;`) + " --allow-root 2>/dev/null"})
		if serr == nil {
			if oldSize, cerr := strconv.Atoi(strings.TrimSpace(sizeOut)); cerr == nil &&
				oldSize > 2048 && len(content)*100 < oldSize*40 {
				return fmt.Sprintf("REJECTED (shrink guard): %s is %d bytes and this write is only %d bytes (<40%%). "+
					"If you meant a small change, use edit_theme_file (search/replace). If you truly intend to replace "+
					"the whole file with something smaller, retry with allow_shrink=true.", filename, oldSize, len(content)), 1, nil
			}
		}
	}

	tmp := "/tmp/wtf_stage.php"
	if err := a.writeContainerFile(site, tmp, []byte(content)); err != nil {
		return "", -1, err
	}
	if strings.HasSuffix(filename, ".php") {
		if lintErr, err := a.phpLint(site, tmp); err != nil {
			return "", -1, err
		} else if lintErr != "" {
			a.execWPCLI(site, []string{"sh", "-c", "rm -f " + tmp})
			return "REJECTED by php -l (file NOT written — fix the syntax error and retry):\n" + lintErr, 1, nil
		}
	}
	return a.installThemeFile(site, tmp, filename)
}

// handleEditThemeFile: unique-match search/replace inside an allowlisted theme
// file, with the same php -l gate on the result. The search string must match
// exactly once — 0 or >1 matches are rejected so edits are deterministic.
func (a *API) handleEditThemeFile(site string, params map[string]string) (string, int, error) {
	filename := params["filename"]
	search := params["search"]
	replace := params["replace"] // may legitimately be empty (deletion)
	if filename == "" || search == "" {
		return "", -1, fmt.Errorf("filename and search are required")
	}
	if !themeFileAllowlist[filename] {
		return "", -1, fmt.Errorf("filename %q is not allowed", filename)
	}
	if len(search) > 65536 || len(replace) > 65536 {
		return "", -1, fmt.Errorf("search/replace too large (max 64KB)")
	}

	// Stage search and replace as files — content never rides command args.
	if err := a.writeContainerFile(site, "/tmp/etf_search", []byte(search)); err != nil {
		return "", -1, err
	}
	if err := a.writeContainerFile(site, "/tmp/etf_replace", []byte(replace)); err != nil {
		return "", -1, err
	}
	phpCode := `
		$path = get_stylesheet_directory() . "/` + filename + `";
		if (!file_exists($path)) { echo "file not found: ` + filename + `"; exit(1); }
		$content = file_get_contents($path);
		$search = file_get_contents("/tmp/etf_search");
		$replace = file_get_contents("/tmp/etf_replace");
		$n = substr_count($content, $search);
		if ($n === 0) { echo "search string not found (0 matches) — read the file and retry with an exact excerpt"; exit(1); }
		if ($n > 1) { echo "search string is ambiguous ($n matches) — include more surrounding context"; exit(1); }
		$updated = str_replace($search, $replace, $content);
		file_put_contents("/tmp/etf_stage", $updated);
		echo "staged";
	`
	out, exit, err := a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(phpCode) + " --allow-root 2>&1"})
	defer a.execWPCLI(site, []string{"sh", "-c", "rm -f /tmp/etf_search /tmp/etf_replace /tmp/etf_stage"})
	if err != nil {
		return "", -1, err
	}
	if exit != 0 {
		return out, exit, nil
	}
	if strings.HasSuffix(filename, ".php") {
		if lintErr, lerr := a.phpLint(site, "/tmp/etf_stage"); lerr != nil {
			return "", -1, lerr
		} else if lintErr != "" {
			return "REJECTED by php -l (edit NOT applied — the replacement breaks the file):\n" + lintErr, 1, nil
		}
	}
	installCode := `
		$path = get_stylesheet_directory() . "/` + filename + `";
		file_put_contents($path, file_get_contents("/tmp/etf_stage"));
		echo "edited ` + filename + `";
	`
	return a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(installCode) + " --allow-root 2>&1"})
}

// verifyPagePathRe keeps the fetched path shell-safe and site-relative.
var verifyPagePathRe = regexp.MustCompile(`^/[a-zA-Z0-9/_\-.?=&%]*$`)

// handleVerifyPage fetches a page through the site's own nginx container and
// returns machine-checkable facts: status, size, title, first h1, tailwind tag.
// splitList parses a comma-separated param into trimmed, non-empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var vpTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// verifyPageChecks returns the list of failed checks for a fetched page body. Pure
// and unit-tested (theme_tools_test.go) so the exact regression that shipped a broken
// store as "verified" — a 301 or a default-blog homepage passing — is locked out.
// Empty result = the page is a real, working page.
func verifyPageChecks(body, status string, expect, expectAbsent []string) []string {
	low := strings.ToLower(body)
	var fails []string
	if status != "200" {
		fails = append(fails, "final_status="+status)
	}
	if len(strings.TrimSpace(body)) < 200 {
		fails = append(fails, "body_too_small")
	}
	for _, m := range []string{"fatal error", "parse error"} {
		if strings.Contains(low, m) {
			fails = append(fails, "php_"+strings.ReplaceAll(m, " ", "_"))
			break
		}
	}
	for _, m := range []string{"just another wordpress site", "hello world!", "welcome to wordpress. this is your first post"} {
		if strings.Contains(low, m) {
			fails = append(fails, "default_wp_content")
			break
		}
	}
	// KSES-escaped markup rendering as visible text ("&lt;h2&gt;Visit Us..."
	// on screen) — happens when content was written without unfiltered_html.
	// A text-presence check can't see it; this string can.
	for _, m := range []string{"&lt;h2&gt;", "&lt;p&gt;", "&lt;strong&gt;"} {
		if strings.Contains(body, m) {
			fails = append(fails, "escaped_html_visible (page shows raw <tags> as text — content was written without unfiltered_html; recreate via create_post)")
			break
		}
	}
	// Model-authored content classes that read as broken to visitors:
	// literal markdown (WordPress renders HTML, not markdown) and bracketed
	// placeholder tokens left in real pages (both founder-reported).
	if textish := stripTags(body); textish != "" {
		if strings.Contains(textish, "**") || strings.Contains(textish, "](http") {
			fails = append(fails, "markdown_visible_as_text (page shows literal ** or []() — WordPress renders HTML, not markdown; rewrite the content via update_post using HTML tags)")
		}
		lowT := strings.ToLower(textish)
		for _, ph := range []string{"placeholder]", "[placeholder", "lorem ipsum", "[contact form", "[your ", "[insert "} {
			if strings.Contains(lowT, ph) {
				fails = append(fails, "placeholder_text_visible (a bracketed placeholder is showing to visitors — replace it with real content or remove it)")
				break
			}
		}
	}
	for _, want := range expect {
		if !strings.Contains(low, strings.ToLower(want)) {
			fails = append(fails, "missing:"+want)
		}
	}
	for _, bad := range expectAbsent {
		if strings.Contains(low, strings.ToLower(bad)) {
			fails = append(fails, "present_but_forbidden:"+bad)
		}
	}
	return fails
}

// handleVerifyPage fetches a page through the site's own nginx and reports whether it
// is a REAL, working page — not merely that something responded. STRENGTHENED 2026-07-08:
// the old version checked only "http 2xx/3xx + a <title>", so it passed a 301 /shop and
// an empty "Hello world" storefront as OK, which let a broken build settle as "verified".
// Now it:
//   - follows redirects (-L) and requires a FINAL 200 (a 301/302 is NOT a pass);
//   - FAILS on PHP Fatal/Parse errors in the body;
//   - FAILS on stock-WordPress content ("Hello world!", "Just another WordPress site",
//     the default first-post body) — the signal that the front page fell back to the blog;
//   - optional expect=a,b,c   — every substring MUST be present (product names, a price);
//   - optional expect_absent=x,y — none may be present.
//
// Content checks run in Go on the fetched body (not shell-grep), so caller-supplied
// expect strings can't inject into a shell command.
// fetchSitePage fetches a path through the site's own nginx container,
// following redirects; returns the body, the FINAL http status, and the
// effective URL. Shared by verify_page and verify_store.
func (a *API) fetchSitePage(site, path string) (body, status, effURL string, err error) {
	target := "http://" + NginxContainerName(site) + path
	// -L follows redirects; -w appends a sentinel with the FINAL status + effective URL
	// after the body, in one request.
	script := "curl -sSL -m 15 -w '___VPMETA___%{http_code}___%{url_effective}___' " + shellQuote(target) + " 2>/dev/null"
	out, _, err := a.execWPCLI(site, []string{"sh", "-c", script})
	if err != nil {
		return "", "", "", err
	}
	body = out
	if i := strings.LastIndex(out, "___VPMETA___"); i >= 0 {
		body = out[:i]
		meta := strings.Trim(out[i+len("___VPMETA___"):], "_")
		if p := strings.SplitN(meta, "___", 2); len(p) > 0 {
			status = strings.TrimSpace(p[0])
			if len(p) > 1 {
				effURL = strings.TrimSpace(p[1])
			}
		}
	}
	return body, status, effURL, nil
}

func (a *API) handleVerifyPage(site string, params map[string]string) (string, int, error) {
	path := params["path"]
	if path == "" {
		path = "/"
	}
	if !verifyPagePathRe.MatchString(path) {
		return "", -1, fmt.Errorf("invalid path")
	}
	body, status, effURL, err := a.fetchSitePage(site, path)
	if err != nil {
		return "", -1, err
	}
	low := strings.ToLower(body)
	fails := verifyPageChecks(body, status, splitList(params["expect"]), splitList(params["expect_absent"]))
	title := ""
	if m := vpTitleRe.FindStringSubmatch(body); m != nil {
		title = strings.TrimSpace(m[1])
	}
	tailwind := strings.Contains(low, "cdn.tailwindcss.com")
	verdict, exit := "OK", 0
	if len(fails) > 0 {
		verdict, exit = "FAILING", 1
	}
	return fmt.Sprintf(
		"verify_page %s: %s\nfinal_status=%s effective_url=%s bytes=%d tailwind=%t\ntitle=%q\nchecks_failed=%v",
		path, verdict, status, effURL, len(body), tailwind, title, fails), exit, nil
}

// requiredThemeTemplates is the activation guard's checklist (design §3.4.3).
var requiredThemeTemplates = []string{"style.css", "index.php", "header.php", "footer.php", "front-page.php"}

// activationGuard refuses to activate a theme missing required templates.
// Returns "" when activation may proceed, else a human-readable refusal.
func (a *API) activationGuard(site, slug string) (string, error) {
	list := strings.Join(requiredThemeTemplates, `","`)
	phpCode := `
		$slug = "` + slug + `";
		$theme = wp_get_theme($slug);
		if (!$theme->exists()) { echo "theme not found"; exit(0); } // let wp-cli produce its own error
		$dir = $theme->get_stylesheet_directory();
		$required = array("` + list + `");
		// WooCommerce routing needs EITHER woocommerce.php (legacy scaffolds)
		// OR the explicit archive/single pair (current scaffolds — shipping
		// woocommerce.php would shadow the styled archive grid).
		if (class_exists("WooCommerce") && !file_exists($dir . "/woocommerce.php")) {
			$required[] = "archive-product.php";
			$required[] = "single-product.php";
		}
		$missing = array();
		foreach ($required as $f) { if (!file_exists($dir . "/" . $f)) { $missing[] = $f; } }
		// Stock/third-party themes ship their own complete structure — only guard
		// scaffolded customs. Detect ours by the deterministic marker file create_theme
		// drops. (Back-compat: themes scaffolded before the marker existed stamped
		// "Hostplane" into style.css's header, so accept that as a secondary signal.)
		$isOurs = file_exists($dir . "/.hostplane-scaffold");
		if (!$isOurs && file_exists($dir . "/style.css")) {
			$isOurs = strpos(file_get_contents($dir . "/style.css"), "Hostplane") !== false;
		}
		if (!empty($missing) && $isOurs) {
			echo "missing:" . implode(",", $missing);
		} else {
			echo "ok";
		}
	`
	out, _, err := a.execWPCLI(site, []string{"sh", "-c", "wp eval " + shellQuote(phpCode) + " --allow-root 2>&1"})
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "missing:") {
		return "activation refused: theme is missing required templates (" +
			strings.TrimPrefix(out, "missing:") +
			"). Finish writing them (write_theme_file), then activate — activating now would show visitors a broken site.", nil
	}
	return "", nil
}


// stripTags reduces rendered HTML to visible text for content-class checks.
func stripTags(html string) string {
	var b strings.Builder
	in := false
	for _, r := range html {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}
