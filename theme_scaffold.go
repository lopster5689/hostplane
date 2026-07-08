package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
)

// curatedDisplayFonts / curatedBodyFonts: the Google-hosted faces create_theme
// may load (D1, design-depth plan). The display face carries the brand
// personality; the body face stays readable. Value = Google Fonts URL spec;
// fallback stacks are derived from the serif set below. Extend deliberately —
// an unknown face would 404 silently and fall back to the browser default.
var curatedDisplayFonts = map[string]string{
	"Plus Jakarta Sans":   "Plus+Jakarta+Sans:wght@400;600;700;800",
	"Fraunces":            "Fraunces:opsz,wght@9..144,400;9..144,600;9..144,700",
	"Playfair Display":    "Playfair+Display:wght@400;600;700",
	"DM Serif Display":    "DM+Serif+Display",
	"Cormorant Garamond":  "Cormorant+Garamond:wght@500;600;700",
	"Marcellus":           "Marcellus",
	"Sora":                "Sora:wght@400;600;700;800",
	"Outfit":              "Outfit:wght@400;600;700;800",
	"Lora":                "Lora:wght@400;500;600;700",
	"Libre Baskerville":   "Libre+Baskerville:wght@400;700",
	"Bricolage Grotesque": "Bricolage+Grotesque:wght@400;600;700;800",
	"Newsreader":          "Newsreader:opsz,wght@6..72,400;6..72,600;6..72,700",
}

var curatedBodyFonts = map[string]string{
	"Inter":         "Inter:wght@400;500;600",
	"Nunito Sans":   "Nunito+Sans:wght@400;600;700",
	"Source Sans 3": "Source+Sans+3:wght@400;600",
	"Work Sans":     "Work+Sans:wght@400;500;600",
	"Karla":         "Karla:wght@400;500;700",
	"Mulish":        "Mulish:wght@400;600;700",
	"Figtree":       "Figtree:wght@400;500;600",
	"Manrope":       "Manrope:wght@400;500;600;700",
	"IBM Plex Sans": "IBM+Plex+Sans:wght@400;500;600",
	"Lora":          "Lora:wght@400;500;600",
}

// serifFaces drives the CSS fallback stack for a chosen face.
var serifFaces = map[string]bool{
	"Fraunces": true, "Playfair Display": true, "DM Serif Display": true,
	"Cormorant Garamond": true, "Marcellus": true, "Lora": true,
	"Libre Baskerville": true, "Newsreader": true,
}

func fontFallback(face string) string {
	if serifFaces[face] {
		return "Georgia, serif"
	}
	return "system-ui, sans-serif"
}

// resolveThemeFonts validates a display/body pairing against the curated set
// and returns the Google Fonts URL to load them. Pure; unit-tested.
func resolveThemeFonts(fontDisplay, fontBody string) (string, error) {
	dspec, ok := curatedDisplayFonts[fontDisplay]
	if !ok {
		return "", fmt.Errorf("font_display %q is not in the curated set — choose one of: Plus Jakarta Sans, Fraunces, Playfair Display, DM Serif Display, Cormorant Garamond, Marcellus, Sora, Outfit, Lora, Libre Baskerville, Bricolage Grotesque, Newsreader", fontDisplay)
	}
	bspec, ok := curatedBodyFonts[fontBody]
	if !ok {
		return "", fmt.Errorf("font_body %q is not in the curated set — choose one of: Inter, Nunito Sans, Source Sans 3, Work Sans, Karla, Mulish, Figtree, Manrope, IBM Plex Sans, Lora", fontBody)
	}
	return "https://fonts.googleapis.com/css2?family=" + dspec + "&family=" + bspec + "&display=swap", nil
}

// handleCreateTheme generates a complete, production-ready WordPress theme
// with all required template files and the Hostplane design system.
func (a *API) handleCreateTheme(site string, params map[string]string) (string, int, error) {
	name := params["name"]
	slug := params["slug"]
	if name == "" || slug == "" {
		return "", -1, fmt.Errorf("name and slug are required")
	}
	if !themeSlugSafeRe.MatchString(slug) {
		return "", -1, fmt.Errorf("slug must be lowercase alphanumeric with hyphens")
	}
	description := params["description"]
	if description == "" {
		description = "Custom theme created by Hostplane AI Editor"
	}
	prefix := strings.ReplaceAll(slug, "-", "_")

	// User can pass custom colors, otherwise use defaults
	primaryColor := params["primary_color"]
	if primaryColor == "" {
		primaryColor = "#2563eb"
	}
	secondaryColor := params["secondary_color"]
	if secondaryColor == "" {
		secondaryColor = "#10b981"
	}
	accentColor := params["accent_color"]
	if accentColor == "" {
		accentColor = "#f59e0b"
	}

	// Typography is a brand decision (D1): validated against the curated set,
	// defaulting to the classic pairing when unspecified.
	fontDisplay := params["font_display"]
	if fontDisplay == "" {
		fontDisplay = "Plus Jakarta Sans"
	}
	fontBody := params["font_body"]
	if fontBody == "" {
		fontBody = "Inter"
	}
	fontsURL, ferr := resolveThemeFonts(fontDisplay, fontBody)
	if ferr != nil {
		return "", -1, ferr
	}

	// Build the PHP script that creates all theme files
	phpScript := buildThemeScript(name, slug, prefix, description, primaryColor, secondaryColor, accentColor,
		fontDisplay, fontBody, fontsURL)

	containerName := PHPContainerName(site)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Step 1: Write PHP script to /tmp/create_theme.php via stdin
	tmpPath := "/tmp/create_theme.php"
	writeCmd := []string{"sh", "-c", "cat > " + tmpPath}
	execResp, err := a.docker.ContainerExecCreate(ctx, containerName, types.ExecConfig{
		Cmd:          writeCmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", -1, fmt.Errorf("exec create (write): %w", err)
	}

	attachResp, err := a.docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return "", -1, fmt.Errorf("exec attach (write): %w", err)
	}

	_, err = io.Copy(attachResp.Conn, bytes.NewReader([]byte(phpScript)))
	attachResp.CloseWrite()
	attachResp.Close()
	if err != nil {
		return "", -1, fmt.Errorf("write script: %w", err)
	}

	// Wait for write to complete
	for i := 0; i < 10; i++ {
		inspect, err := a.docker.ContainerExecInspect(ctx, execResp.ID)
		if err != nil {
			break
		}
		if !inspect.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Step 2: Execute the script via wp eval-file
	evalCmd := []string{"sh", "-c", "wp eval-file " + tmpPath + " --allow-root 2>&1"}
	output, exitCode, err := a.execWPCLI(site, evalCmd)
	if err != nil {
		return "", -1, fmt.Errorf("eval-file: %w", err)
	}

	// Cleanup
	cleanCmd := []string{"sh", "-c", "rm -f " + tmpPath}
	a.execWPCLI(site, cleanCmd)

	if exitCode != 0 {
		log.Printf("create_theme failed for site %s: %s", site, output)
	}

	return output, exitCode, nil
}

// buildThemeScript generates the PHP script that creates all theme files.
func buildThemeScript(name, slug, prefix, description, primary, secondary, accent,
	fontDisplay, fontBody, fontsURL string) string {
	return `<?php
$slug = '` + slug + `';
$name = '` + strings.ReplaceAll(name, "'", "\\'") + `';
$desc = '` + strings.ReplaceAll(description, "'", "\\'") + `';
$prefix = '` + prefix + `';
$primary = '` + primary + `';
$secondary = '` + secondary + `';
$accent = '` + accent + `';
$font_display = '` + fontDisplay + `';
$font_body = '` + fontBody + `';
$display_fallback = '` + fontFallback(fontDisplay) + `';
$body_fallback = 'system-ui, sans-serif';
$fonts_url = '` + fontsURL + `';

$dir = WP_CONTENT_DIR . "/themes/" . $slug;
if (is_dir($dir)) { echo "Error: theme already exists"; exit(1); }
mkdir($dir, 0755, true);
// Reliable scaffold marker so the activation guard can tell OUR customs (which it
// MUST guard for template completeness) from stock/third-party themes (which it must
// not). Deterministic — unlike the old style.css "Hostplane" string the model can override.
file_put_contents($dir . "/.hostplane-scaffold", "hostplane-ai-editor");

// ═══════════════════════════════════════════════════════════════
// style.css — Design system with CSS custom properties
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/style.css", <<<CSS
/*
Theme Name: $name
Description: $desc
Version: 1.0
*/

:root {
  /* Brand decisions (authored per store via create_theme). */
  --color-primary: $primary;
  --color-secondary: $secondary;
  --color-accent: $accent;
  --color-text: #1e293b;
  --color-muted: #64748b;
  --color-surface: #f8fafc;
  --color-card: #ffffff;
  /* Derived — one decision computes its states (never hand-pick these). */
  --color-primary-hover: color-mix(in oklab, $primary, black 10%);
  --color-secondary-hover: color-mix(in oklab, $secondary, black 10%);
  --color-border: color-mix(in oklab, var(--color-text) 12%, var(--color-surface));
  --color-footer: color-mix(in oklab, var(--color-text) 90%, var(--color-primary));
  --font-heading: '$font_display', $display_fallback;
  --font-body: '$font_body', $body_fallback;
}

body {
  font-family: var(--font-body);
  color: var(--color-text);
  background: var(--color-surface);
  margin: 0;
}

h1, h2, h3, h4, h5, h6 {
  font-family: var(--font-heading);
}

/* Classic WooCommerce (cart / checkout / account) — server-rendered pages
   styled through the same tokens as the rest of the store. */
.woocommerce table.shop_table { width: 100%; border-collapse: collapse; background: var(--color-card); border: 1px solid var(--color-border); }
.woocommerce table.shop_table th, .woocommerce table.shop_table td { padding: 12px 16px; border-bottom: 1px solid var(--color-border); text-align: left; }
.woocommerce table.shop_table img { width: 64px; height: 64px; object-fit: cover; border-radius: 8px; }
.woocommerce a.button, .woocommerce button.button, .woocommerce input.button, .woocommerce #place_order { background: var(--color-primary); color: #ffffff; border: 0; border-radius: 8px; padding: 12px 20px; font-weight: 600; cursor: pointer; text-decoration: none; display: inline-block; }
.woocommerce a.button:hover, .woocommerce button.button:hover, .woocommerce input.button:hover, .woocommerce #place_order:hover { background: var(--color-primary-hover); color: #ffffff; }
.woocommerce a.button.alt, .woocommerce button.button.alt, .woocommerce input.button.alt, .woocommerce a.checkout-button { background: var(--color-secondary); color: #ffffff; }
.woocommerce a.button.alt:hover, .woocommerce button.button.alt:hover, .woocommerce input.button.alt:hover, .woocommerce a.checkout-button:hover { background: var(--color-secondary-hover); color: #ffffff; }
.woocommerce input.input-text, .woocommerce select, .woocommerce textarea { border: 1px solid var(--color-border); border-radius: 8px; padding: 10px 12px; width: 100%; background: var(--color-card); font-family: var(--font-body); }
.woocommerce form .form-row { margin-bottom: 14px; }
.woocommerce form .form-row label { display: block; margin-bottom: 4px; font-size: 0.85rem; color: var(--color-muted); }
.woocommerce .woocommerce-message, .woocommerce .woocommerce-info { background: var(--color-surface); border-left: 4px solid var(--color-primary); padding: 12px 16px; margin-bottom: 16px; }
.woocommerce .woocommerce-error { background: #fbeeec; border-left: 4px solid #b3402e; padding: 12px 16px; margin-bottom: 16px; list-style: none; }
.woocommerce .cart_totals, .woocommerce #order_review { background: var(--color-card); border: 1px solid var(--color-border); border-radius: 12px; padding: 20px; }
.woocommerce ul#shipping_method { list-style: none; padding: 0; margin: 0; }
.woocommerce .quantity input.qty { width: 72px; padding: 8px; border: 1px solid var(--color-border); border-radius: 8px; }
.woocommerce .coupon input.input-text { width: auto; }
CSS
);

// ═══════════════════════════════════════════════════════════════
// functions.php — Theme setup with WooCommerce + Google Fonts
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/functions.php", <<<'FUNC'
<?php
function THEME_PREFIX_setup() {
    add_theme_support('title-tag');
    add_theme_support('post-thumbnails');
    add_theme_support('woocommerce');
    add_theme_support('custom-logo');
    add_theme_support('html5', array('search-form', 'comment-form', 'comment-list', 'gallery', 'caption'));
    register_nav_menus(array(
        'primary' => 'Primary Menu',
        'footer'  => 'Footer Menu',
    ));
}
add_action('after_setup_theme', 'THEME_PREFIX_setup');

function THEME_PREFIX_scripts() {
    // Version by mtime: theme slugs recur across rebuilds and Cloudflare
    // caches static CSS aggressively — a fixed ?ver served a day-old palette
    // to browsers while the origin was correct (found live 2026-07-08).
    wp_enqueue_style('THEME_PREFIX-style', get_stylesheet_uri(), array(), (string) @filemtime(get_stylesheet_directory() . '/style.css'));
    wp_enqueue_style('THEME_PREFIX-fonts', '__FONTS_URL__', array(), null);
}
add_action('wp_enqueue_scripts', 'THEME_PREFIX_scripts');

// Register widget areas
function THEME_PREFIX_widgets() {
    register_sidebar(array(
        'name' => 'Footer 1',
        'id' => 'footer-1',
        'before_widget' => '<div class="mb-4">',
        'after_widget' => '</div>',
    ));
}
add_action('widgets_init', 'THEME_PREFIX_widgets');
FUNC
);
// Replace placeholders with the actual prefix + the curated fonts URL
$func_content = file_get_contents($dir . "/functions.php");
$func_content = str_replace('THEME_PREFIX', $prefix, $func_content);
$func_content = str_replace('__FONTS_URL__', $fonts_url, $func_content);
file_put_contents($dir . "/functions.php", $func_content);

// ═══════════════════════════════════════════════════════════════
// header.php — Tailwind CDN BEFORE wp_head, sticky nav, cart
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/header.php", <<<'HEADER'
<!DOCTYPE html>
<html <?php language_attributes(); ?>>
<head>
    <meta charset="<?php bloginfo('charset'); ?>">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <script src="https://cdn.tailwindcss.com"></script>
    <script>
    /* Token bridge (design-depth D1): every brand color/type decision lives in
       style.css :root custom properties; Tailwind classes consume them via
       this mapping. Templates use ONLY token classes (bg-primary, text-ink,
       border-line...) — never raw palette classes — so one palette change
       restyles the whole store. */
    tailwind.config = {
        theme: {
            extend: {
                colors: {
                    primary: 'var(--color-primary)',
                    'primary-hover': 'var(--color-primary-hover)',
                    secondary: 'var(--color-secondary)',
                    'secondary-hover': 'var(--color-secondary-hover)',
                    accent: 'var(--color-accent)',
                    ink: 'var(--color-text)',
                    muted: 'var(--color-muted)',
                    surface: 'var(--color-surface)',
                    card: 'var(--color-card)',
                    line: 'var(--color-border)',
                    footer: 'var(--color-footer)',
                },
                fontFamily: {
                    heading: ['__FONT_DISPLAY__', 'sans-serif'],
                    body: ['__FONT_BODY__', 'sans-serif'],
                },
            }
        }
    }
    </script>
    <?php wp_head(); ?>
</head>
<body <?php body_class('bg-surface text-ink font-body antialiased'); ?>>
<?php wp_body_open(); ?>

<header class="sticky top-0 z-50 bg-[color-mix(in_srgb,var(--color-card)_85%,transparent)] backdrop-blur-md border-b border-line">
    <div class="max-w-7xl mx-auto px-4 md:px-6 h-16 flex items-center justify-between">
        <a href="<?php echo home_url(); ?>" class="text-xl font-heading font-extrabold tracking-tight">
            <?php bloginfo('name'); ?>
        </a>

        <nav class="hidden md:flex items-center space-x-8">
            <?php
            if (has_nav_menu('primary')) {
                wp_nav_menu(array(
                    'theme_location' => 'primary',
                    'container' => false,
                    'menu_class' => 'flex space-x-8',
                    'fallback_cb' => false,
                ));
            } else {
                echo '<a href="' . home_url('/shop') . '" class="text-sm font-medium text-muted hover:text-ink">Shop</a>';
            }
            ?>
        </nav>

        <div class="flex items-center space-x-4">
            <?php if (class_exists('WooCommerce')): ?>
            <a href="<?php echo wc_get_cart_url(); ?>" class="relative p-2 text-muted hover:text-ink">
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M16 11V7a4 4 0 00-8 0v4M5 9h14l1 12H4L5 9z"/>
                </svg>
                <?php $count = WC()->cart->get_cart_contents_count(); if ($count > 0): ?>
                <span class="absolute -top-1 -right-1 bg-primary text-white text-xs w-5 h-5 flex items-center justify-center rounded-full"><?php echo $count; ?></span>
                <?php endif; ?>
            </a>
            <?php endif; ?>
            <button class="md:hidden p-2 text-muted hover:text-ink" aria-label="Menu" aria-expanded="false"
                onclick="var m=document.getElementById('hp-mobile-nav');m.classList.toggle('hidden');this.setAttribute('aria-expanded', m.classList.contains('hidden')?'false':'true');">
                <svg class="w-6 h-6" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 6h16M4 12h16M4 18h16"/></svg>
            </button>
        </div>
    </div>
    <div id="hp-mobile-nav" class="hidden md:hidden border-t border-line bg-card px-4 py-4">
        <?php
        if (has_nav_menu('primary')) {
            wp_nav_menu(array(
                'theme_location' => 'primary',
                'container' => false,
                'menu_class' => 'flex flex-col gap-4 text-sm font-medium',
                'fallback_cb' => false,
            ));
        } else {
            echo '<a href="' . home_url('/shop') . '" class="text-sm font-medium text-muted">Shop</a>';
        }
        ?>
    </div>
</header>
HEADER
);
$head_content = file_get_contents($dir . "/header.php");
$head_content = str_replace('__FONT_DISPLAY__', $font_display, $head_content);
$head_content = str_replace('__FONT_BODY__', $font_body, $head_content);
file_put_contents($dir . "/header.php", $head_content);

// ═══════════════════════════════════════════════════════════════
// footer.php — Dark 3-column footer
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/footer.php", <<<'FOOTER'
<footer class="bg-footer text-white mt-20">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-16">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-12 mb-12">
            <div>
                <h3 class="text-lg font-heading font-bold mb-4"><?php bloginfo('name'); ?></h3>
                <p class="text-white/60 text-sm leading-relaxed"><?php bloginfo('description'); ?></p>
            </div>
            <div>
                <h4 class="font-semibold mb-4 text-sm uppercase tracking-wider text-white/50">Quick Links</h4>
                <ul class="space-y-3">
                    <li><a href="<?php echo home_url(); ?>" class="text-white/70 hover:text-white text-sm transition">Home</a></li>
                    <li><a href="<?php echo home_url('/shop'); ?>" class="text-white/70 hover:text-white text-sm transition">Shop</a></li>
                    <?php if (class_exists('WooCommerce')): ?>
                    <li><a href="<?php echo wc_get_cart_url(); ?>" class="text-white/70 hover:text-white text-sm transition">Cart</a></li>
                    <?php endif; ?>
                </ul>
            </div>
            <div>
                <h4 class="font-semibold mb-4 text-sm uppercase tracking-wider text-white/50">Contact</h4>
                <p class="text-white/60 text-sm">Get in touch with us for any questions about your order.</p>
            </div>
        </div>
        <div class="border-t border-white/10 pt-8 text-center">
            <p class="text-white/50 text-sm">&copy; <?php echo date('Y'); ?> <?php bloginfo('name'); ?>. All rights reserved.</p>
        </div>
    </div>
</footer>
<?php wp_footer(); ?>
</body>
</html>
FOOTER
);

// ═══════════════════════════════════════════════════════════════
// front-page.php — Hero + Featured Products + Value Props
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/front-page.php", <<<'FRONTPAGE'
<?php get_header();
// Section-driven homepage (design-depth D3): the model composes typed
// sections via set_home_sections; each type renders deterministic
// token-class markup here. No sections set -> legacy 3-block composition.
$hp_sections = json_decode(get_option('hostplane_home_sections', '[]'), true);
if (!is_array($hp_sections)) { $hp_sections = array(); }
if (empty($hp_sections)) {
    $hp_sections = array(
        array('type' => '_legacy_hero'),
        array('type' => 'bestsellers', 'title' => 'Featured Products'),
        array('type' => '_legacy_props'),
    );
}
foreach ($hp_sections as $hp_s):
    $t = isset($hp_s['type']) ? $hp_s['type'] : '';
    if ($t === 'hero_image'):
        $hp_img = wp_get_attachment_image_src((int) $hp_s['image_id'], 'full');
?>
<section class="relative overflow-hidden">
    <?php if ($hp_img): ?>
    <img src="<?php echo esc_url($hp_img[0]); ?>" alt="" class="absolute inset-0 w-full h-full object-cover">
    <div class="absolute inset-0 bg-black/45"></div>
    <?php endif; ?>
    <div class="relative max-w-7xl mx-auto px-4 md:px-6 py-28 md:py-40 <?php echo $hp_img ? 'text-white' : ''; ?>">
        <h1 class="text-4xl md:text-6xl font-heading font-extrabold tracking-tight mb-4 max-w-3xl"><?php echo esc_html(!empty($hp_s['headline']) ? $hp_s['headline'] : get_bloginfo('name')); ?></h1>
        <?php if (!empty($hp_s['sub'])): ?><p class="text-xl <?php echo $hp_img ? 'text-white/85' : 'text-muted'; ?> mb-8 max-w-2xl leading-relaxed"><?php echo esc_html($hp_s['sub']); ?></p><?php endif; ?>
        <a href="<?php echo home_url('/shop'); ?>" class="inline-block bg-primary text-white px-8 py-3.5 rounded-lg font-semibold hover:bg-primary-hover transition shadow-lg">Shop Now</a>
    </div>
</section>
<?php elseif ($t === 'story'): ?>
<section class="bg-card">
    <div class="max-w-4xl mx-auto px-4 md:px-6 py-16 md:py-24 text-center">
        <h2 class="text-2xl md:text-3xl font-heading font-bold mb-6"><?php echo esc_html($hp_s['title']); ?></h2>
        <p class="text-muted leading-relaxed text-lg"><?php echo esc_html($hp_s['text']); ?></p>
    </div>
</section>
<?php elseif ($t === 'category_strip' && class_exists('WooCommerce')):
    $hp_cats = get_terms(array('taxonomy' => 'product_cat', 'hide_empty' => true, 'number' => 6));
    if (!empty($hp_cats) && !is_wp_error($hp_cats)): ?>
<section class="max-w-7xl mx-auto px-4 md:px-6 py-12">
    <div class="flex flex-wrap gap-3 justify-center">
        <?php foreach ($hp_cats as $hp_c): ?>
        <a href="<?php echo esc_url(get_term_link($hp_c)); ?>" class="border border-line bg-card px-5 py-2.5 rounded-full text-sm font-semibold hover:bg-surface transition"><?php echo esc_html($hp_c->name); ?></a>
        <?php endforeach; ?>
    </div>
</section>
<?php endif; elseif ($t === 'bestsellers' && class_exists('WooCommerce')): ?>
<section class="max-w-7xl mx-auto px-4 md:px-6 py-16 md:py-24">
    <div class="flex items-end justify-between mb-10">
        <div>
            <h2 class="text-2xl md:text-3xl font-heading font-bold"><?php echo esc_html(!empty($hp_s['title']) ? $hp_s['title'] : 'Featured Products'); ?></h2>
        </div>
        <a href="<?php echo home_url('/shop'); ?>" class="text-primary font-semibold text-sm hover:underline hidden md:block">View All &rarr;</a>
    </div>
    <div class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-6">
        <?php
        $hp_products = wc_get_products(array('limit' => 8, 'status' => 'publish', 'orderby' => 'date'));
        foreach ($hp_products as $hp_p):
            $hp_pimg = wp_get_attachment_image_src($hp_p->get_image_id(), 'medium');
        ?>
        <div class="bg-card rounded-xl shadow-sm border border-line overflow-hidden group hover:shadow-md hover:scale-[1.02] transition-all duration-300">
            <div class="aspect-square bg-surface overflow-hidden">
                <?php if ($hp_pimg): ?>
                    <img src="<?php echo $hp_pimg[0]; ?>" alt="<?php echo esc_attr($hp_p->get_name()); ?>" class="w-full h-full object-cover group-hover:scale-110 transition-transform duration-500">
                <?php endif; ?>
            </div>
            <div class="p-4">
                <h3 class="font-semibold text-sm mb-1 line-clamp-2"><?php echo $hp_p->get_name(); ?></h3>
                <p class="text-primary font-bold"><?php echo $hp_p->get_price_html(); ?></p>
                <a href="<?php echo $hp_p->get_permalink(); ?>" class="mt-3 block w-full bg-secondary text-white text-center py-2 rounded-lg text-sm font-semibold hover:bg-secondary-hover transition">View Product</a>
            </div>
        </div>
        <?php endforeach; ?>
    </div>
</section>
<?php elseif ($t === 'hours_location'): ?>
<section class="bg-card border-t border-line">
    <div class="max-w-5xl mx-auto px-4 md:px-6 py-16 grid md:grid-cols-2 gap-10">
        <div>
            <h2 class="text-xl font-heading font-bold mb-4">Visit Us</h2>
            <?php if (!empty($hp_s['address'])): ?><p class="text-muted mb-2"><?php echo esc_html($hp_s['address']); ?></p><?php endif; ?>
            <?php if (!empty($hp_s['phone'])): ?><p class="text-muted"><a href="tel:<?php echo esc_attr(preg_replace('/[^0-9+]/', '', $hp_s['phone'])); ?>" class="text-primary font-semibold"><?php echo esc_html($hp_s['phone']); ?></a></p><?php endif; ?>
        </div>
        <?php if (!empty($hp_s['hours']) && is_array($hp_s['hours'])): ?>
        <div>
            <h2 class="text-xl font-heading font-bold mb-4">Opening Hours</h2>
            <ul class="space-y-2">
                <?php foreach ($hp_s['hours'] as $hp_h): ?>
                <li class="text-muted text-sm border-b border-line pb-2"><?php echo esc_html($hp_h); ?></li>
                <?php endforeach; ?>
            </ul>
        </div>
        <?php endif; ?>
    </div>
</section>
<?php elseif ($t === 'gallery'): ?>
<section class="max-w-7xl mx-auto px-4 md:px-6 py-16">
    <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
        <?php foreach ((array) $hp_s['image_ids'] as $hp_gid):
            $hp_g = wp_get_attachment_image_src((int) $hp_gid, 'large'); if (!$hp_g) continue; ?>
        <div class="aspect-square rounded-xl overflow-hidden bg-surface">
            <img src="<?php echo esc_url($hp_g[0]); ?>" alt="" class="w-full h-full object-cover hover:scale-105 transition-transform duration-500">
        </div>
        <?php endforeach; ?>
    </div>
</section>
<?php elseif ($t === 'offer_banner'): ?>
<section class="bg-accent">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-3 text-center">
        <p class="text-white text-sm font-semibold tracking-wide"><?php echo esc_html($hp_s['text']); ?></p>
    </div>
</section>
<?php elseif ($t === '_legacy_hero'): ?>
<section class="relative overflow-hidden bg-card">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-20 md:py-32">
        <div class="max-w-3xl">
            <h1 class="text-4xl md:text-6xl font-heading font-extrabold tracking-tight mb-6"><?php bloginfo('name'); ?></h1>
            <p class="text-xl text-muted mb-8 leading-relaxed"><?php bloginfo('description'); ?></p>
            <div class="flex flex-col sm:flex-row gap-4">
                <a href="<?php echo home_url('/shop'); ?>" class="bg-primary text-white px-8 py-3.5 rounded-lg font-semibold text-center hover:bg-primary-hover transition shadow-lg">Shop Now</a>
            </div>
        </div>
    </div>
</section>
<?php elseif ($t === '_legacy_props'): ?>
<section class="bg-card border-t border-line">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-16 md:py-24">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-8">
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-surface rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-primary" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 8h14M5 8a2 2 0 110-4h14a2 2 0 110 4M5 8v10a2 2 0 002 2h10a2 2 0 002-2V8m-9 4h4"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">Free Shipping</h3>
                <p class="text-muted text-sm">Free delivery on qualifying orders</p>
            </div>
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-surface rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-secondary" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">Secure Payments</h3>
                <p class="text-muted text-sm">100% secure payment processing</p>
            </div>
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-surface rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-accent" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M18.364 5.636l-3.536 3.536m0 5.656l3.536 3.536M9.172 9.172L5.636 5.636m3.536 9.192l-3.536 3.536M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-5 0a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">Quality Guaranteed</h3>
                <p class="text-muted text-sm">Made with care, every single time</p>
            </div>
        </div>
    </div>
</section>
<?php endif; endforeach; ?>

<?php get_footer(); ?>
FRONTPAGE
);

// NOTE deliberately NO woocommerce.php: WooCommerce's template loader gives
// woocommerce.php priority over archive-product.php, so shipping it would
// shadow the styled Tailwind shop grid below with woocommerce_content()'s
// wc-CSS-dependent loop (the collapsed /shop bug, found twice — 2026-07-08).
// With add_theme_support('woocommerce') + explicit archive-product.php and
// single-product.php, the loader uses those directly.

// ═══════════════════════════════════════════════════════════════
// page.php — Generic page template
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/page.php", <<<'PAGE'
<?php get_header(); ?>
<main class="max-w-4xl mx-auto px-4 md:px-6 py-12">
    <?php while (have_posts()) : the_post(); ?>
    <article>
        <h1 class="text-3xl md:text-4xl font-heading font-bold mb-8"><?php the_title(); ?></h1>
        <div class="prose max-w-none">
            <?php the_content(); ?>
        </div>
    </article>
    <?php endwhile; ?>
</main>
<?php get_footer(); ?>
PAGE
);

// ═══════════════════════════════════════════════════════════════
// single.php — Blog post template
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/single.php", <<<'SINGLE'
<?php get_header(); ?>
<main class="max-w-4xl mx-auto px-4 md:px-6 py-12">
    <?php while (have_posts()) : the_post(); ?>
    <article>
        <header class="mb-8">
            <time class="text-sm text-muted font-medium"><?php echo get_the_date(); ?></time>
            <h1 class="text-3xl md:text-4xl font-heading font-bold mt-2"><?php the_title(); ?></h1>
        </header>
        <?php if (has_post_thumbnail()): ?>
        <div class="mb-8 rounded-xl overflow-hidden">
            <?php the_post_thumbnail('large', array('class' => 'w-full h-auto')); ?>
        </div>
        <?php endif; ?>
        <div class="prose max-w-none">
            <?php the_content(); ?>
        </div>
    </article>
    <?php endwhile; ?>
</main>
<?php get_footer(); ?>
SINGLE
);

// ═══════════════════════════════════════════════════════════════
// single-product.php — WooCommerce product detail
// ═══════════════════════════════════════════════════════════════
// Self-contained Tailwind product detail — same rationale as archive-product:
// wc's content-single-product template depends on WooCommerce CSS the Tailwind
// preflight strips (rendered as an unstyled floating column). The add-to-cart
// form is wc's own (cart/variations keep working); arbitrary-variant classes
// style it without wc CSS.
file_put_contents($dir . "/single-product.php", <<<'PRODUCT'
<?php get_header(); ?>
<main class="max-w-7xl mx-auto px-4 md:px-6 py-12">
    <?php while (have_posts()) : the_post(); global $product; if (!($product instanceof WC_Product) && function_exists('wc_get_product')) { $product = wc_get_product(get_the_ID()); } ?>
    <div class="grid md:grid-cols-2 gap-10 items-start">
        <div class="bg-card rounded-xl shadow-sm border border-line overflow-hidden">
            <?php if (has_post_thumbnail()): ?>
                <?php the_post_thumbnail('large', array('class' => 'w-full h-auto object-cover')); ?>
            <?php else: ?>
                <div class="aspect-square bg-surface"></div>
            <?php endif; ?>
        </div>
        <div class="flex flex-col gap-4">
            <h1 class="text-3xl md:text-4xl font-heading font-bold"><?php the_title(); ?></h1>
            <div class="text-2xl text-primary font-bold"><?php echo $product ? $product->get_price_html() : ''; ?></div>
            <div class="prose max-w-none"><?php the_content(); ?></div>
            <div class="mt-2 [&_form.cart]:flex [&_form.cart]:items-center [&_form.cart]:gap-3 [&_.quantity_input]:w-20 [&_.quantity_input]:border [&_.quantity_input]:border-line [&_.quantity_input]:rounded-lg [&_.quantity_input]:px-3 [&_.quantity_input]:py-2 [&_form.cart_button]:bg-secondary [&_form.cart_button]:text-white [&_form.cart_button]:px-6 [&_form.cart_button]:py-3 [&_form.cart_button]:rounded-lg [&_form.cart_button]:font-semibold [&_form.cart_button]:border-0 [&_form.cart_button]:cursor-pointer">
                <?php if ($product) { woocommerce_template_single_add_to_cart(); } ?>
            </div>
        </div>
    </div>
    <?php endwhile; ?>
</main>
<?php get_footer(); ?>
PRODUCT
);

// ═══════════════════════════════════════════════════════════════
// archive-product.php — Shop / category archive
// ═══════════════════════════════════════════════════════════════
// Self-contained Tailwind product grid — deliberately does NOT use
// woocommerce_product_loop_start()/content-product templates: their layout
// depends on WooCommerce's own stylesheet, which the Tailwind CDN preflight
// clobbers (the collapsed /shop grid bug, 2026-07-08). Same card system as
// front-page.php, so the shop page matches the storefront by construction.
file_put_contents($dir . "/archive-product.php", <<<'ARCHIVE'
<?php get_header(); ?>
<main class="max-w-7xl mx-auto px-4 md:px-6 py-12">
    <header class="mb-10">
        <?php if (is_shop()): ?>
            <h1 class="text-3xl font-heading font-bold">Shop</h1>
        <?php else: ?>
            <h1 class="text-3xl font-heading font-bold"><?php woocommerce_page_title(); ?></h1>
        <?php endif; ?>
        <div class="w-16 h-1 bg-primary rounded mt-3"></div>
    </header>
    <?php if (have_posts()): ?>
    <div class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-6">
        <?php while (have_posts()): the_post(); global $product; ?>
        <a href="<?php the_permalink(); ?>" class="bg-card rounded-xl shadow-sm border border-line hover:shadow-md hover:scale-[1.02] transition-all overflow-hidden flex flex-col">
            <div class="aspect-square bg-surface overflow-hidden">
                <?php if (has_post_thumbnail()): ?>
                    <?php the_post_thumbnail('woocommerce_thumbnail', array('class' => 'w-full h-full object-cover')); ?>
                <?php endif; ?>
            </div>
            <div class="p-4 md:p-6 flex flex-col gap-2 flex-1">
                <h2 class="font-heading font-semibold text-ink"><?php the_title(); ?></h2>
                <?php if ($product instanceof WC_Product): ?>
                <div class="text-primary font-bold mt-auto"><?php echo $product->get_price_html(); ?></div>
                <span class="bg-secondary text-white text-center px-4 py-2 rounded-lg font-semibold text-sm hover:bg-secondary-hover transition">View Product</span>
                <?php endif; ?>
            </div>
        </a>
        <?php endwhile; ?>
    </div>
    <nav class="mt-10 flex justify-center gap-2 [&_a]:px-4 [&_a]:py-2 [&_a]:rounded-lg [&_a]:border [&_a]:border-line [&_span.current]:px-4 [&_span.current]:py-2 [&_span.current]:rounded-lg [&_span.current]:bg-primary [&_span.current]:text-white">
        <?php woocommerce_pagination(); ?>
    </nav>
    <?php else: ?>
    <p class="text-muted">No products found.</p>
    <?php endif; ?>
</main>
<?php get_footer(); ?>
ARCHIVE
);

// ═══════════════════════════════════════════════════════════════
// 404.php — Error page
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/404.php", <<<'NOTFOUND'
<?php get_header(); ?>
<main class="max-w-4xl mx-auto px-4 md:px-6 py-24 text-center">
    <h1 class="text-6xl font-heading font-extrabold text-line mb-4">404</h1>
    <p class="text-xl text-muted mb-8">Page not found</p>
    <a href="<?php echo home_url(); ?>" class="bg-primary text-white px-6 py-3 rounded-lg font-semibold hover:bg-primary-hover transition">
        Go Home
    </a>
</main>
<?php get_footer(); ?>
NOTFOUND
);

// ═══════════════════════════════════════════════════════════════
// index.php — Fallback template
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/index.php", <<<'INDEX'
<?php get_header(); ?>
<main class="max-w-4xl mx-auto px-4 md:px-6 py-12">
    <?php if (have_posts()) : while (have_posts()) : the_post(); ?>
    <article class="mb-12 pb-12 border-b border-line last:border-0">
        <h2 class="text-2xl font-heading font-bold mb-3">
            <a href="<?php the_permalink(); ?>" class="hover:text-primary transition"><?php the_title(); ?></a>
        </h2>
        <time class="text-sm text-muted"><?php echo get_the_date(); ?></time>
        <div class="mt-4 text-muted leading-relaxed">
            <?php the_excerpt(); ?>
        </div>
    </article>
    <?php endwhile; else : ?>
    <p class="text-muted">No content found.</p>
    <?php endif; ?>
</main>
<?php get_footer(); ?>
INDEX
);

echo "Theme '$name' created at themes/$slug with 9 template files + Tailwind CSS design system";
`
}
