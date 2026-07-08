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

	// Build the PHP script that creates all theme files
	phpScript := buildThemeScript(name, slug, prefix, description, primaryColor, secondaryColor, accentColor)

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
func buildThemeScript(name, slug, prefix, description, primary, secondary, accent string) string {
	return `<?php
$slug = '` + slug + `';
$name = '` + strings.ReplaceAll(name, "'", "\\'") + `';
$desc = '` + strings.ReplaceAll(description, "'", "\\'") + `';
$prefix = '` + prefix + `';
$primary = '` + primary + `';
$secondary = '` + secondary + `';
$accent = '` + accent + `';

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
  --color-primary: $primary;
  --color-secondary: $secondary;
  --color-accent: $accent;
  --color-text: #1e293b;
  --color-muted: #64748b;
  --color-surface: #f8fafc;
  --color-card: #ffffff;
  --font-heading: 'Plus Jakarta Sans', sans-serif;
  --font-body: 'Inter', sans-serif;
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
    wp_enqueue_style('THEME_PREFIX-style', get_stylesheet_uri(), array(), '1.0');
    wp_enqueue_style('THEME_PREFIX-fonts', 'https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;600;700;800&family=Inter:wght@400;500;600&display=swap', array(), null);
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
// Replace placeholder with actual prefix
$func_content = file_get_contents($dir . "/functions.php");
$func_content = str_replace('THEME_PREFIX', $prefix, $func_content);
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
    tailwind.config = {
        theme: {
            extend: {
                fontFamily: {
                    heading: ['Plus Jakarta Sans', 'sans-serif'],
                    body: ['Inter', 'sans-serif'],
                },
            }
        }
    }
    </script>
    <?php wp_head(); ?>
</head>
<body <?php body_class('bg-slate-50 text-slate-900 font-body antialiased'); ?>>
<?php wp_body_open(); ?>

<header class="sticky top-0 z-50 bg-white/80 backdrop-blur-md border-b border-slate-100">
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
                echo '<a href="' . home_url('/shop') . '" class="text-sm font-medium text-slate-600 hover:text-slate-900">Shop</a>';
            }
            ?>
        </nav>

        <div class="flex items-center space-x-4">
            <?php if (class_exists('WooCommerce')): ?>
            <a href="<?php echo wc_get_cart_url(); ?>" class="relative p-2 text-slate-600 hover:text-slate-900">
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M16 11V7a4 4 0 00-8 0v4M5 9h14l1 12H4L5 9z"/>
                </svg>
                <?php $count = WC()->cart->get_cart_contents_count(); if ($count > 0): ?>
                <span class="absolute -top-1 -right-1 bg-blue-600 text-white text-xs w-5 h-5 flex items-center justify-center rounded-full"><?php echo $count; ?></span>
                <?php endif; ?>
            </a>
            <?php endif; ?>
        </div>
    </div>
</header>
HEADER
);

// ═══════════════════════════════════════════════════════════════
// footer.php — Dark 3-column footer
// ═══════════════════════════════════════════════════════════════
file_put_contents($dir . "/footer.php", <<<'FOOTER'
<footer class="bg-slate-900 text-white mt-20">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-16">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-12 mb-12">
            <div>
                <h3 class="text-lg font-heading font-bold mb-4"><?php bloginfo('name'); ?></h3>
                <p class="text-slate-400 text-sm leading-relaxed"><?php bloginfo('description'); ?></p>
            </div>
            <div>
                <h4 class="font-semibold mb-4 text-sm uppercase tracking-wider text-slate-500">Quick Links</h4>
                <ul class="space-y-3">
                    <li><a href="<?php echo home_url(); ?>" class="text-slate-300 hover:text-white text-sm transition">Home</a></li>
                    <li><a href="<?php echo home_url('/shop'); ?>" class="text-slate-300 hover:text-white text-sm transition">Shop</a></li>
                    <?php if (class_exists('WooCommerce')): ?>
                    <li><a href="<?php echo wc_get_cart_url(); ?>" class="text-slate-300 hover:text-white text-sm transition">Cart</a></li>
                    <?php endif; ?>
                </ul>
            </div>
            <div>
                <h4 class="font-semibold mb-4 text-sm uppercase tracking-wider text-slate-500">Contact</h4>
                <p class="text-slate-400 text-sm">Get in touch with us for any questions about your order.</p>
            </div>
        </div>
        <div class="border-t border-slate-800 pt-8 text-center">
            <p class="text-slate-500 text-sm">&copy; <?php echo date('Y'); ?> <?php bloginfo('name'); ?>. All rights reserved.</p>
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
<?php get_header(); ?>

<!-- Hero Section -->
<section class="relative overflow-hidden bg-white">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-20 md:py-32">
        <div class="max-w-3xl">
            <h1 class="text-4xl md:text-6xl font-heading font-extrabold tracking-tight mb-6">
                <?php bloginfo('name'); ?>
            </h1>
            <p class="text-xl text-slate-600 mb-8 leading-relaxed">
                <?php bloginfo('description'); ?>
            </p>
            <div class="flex flex-col sm:flex-row gap-4">
                <a href="<?php echo home_url('/shop'); ?>" class="bg-blue-600 text-white px-8 py-3.5 rounded-lg font-semibold text-center hover:bg-blue-700 transition shadow-lg shadow-blue-600/25">
                    Shop Now
                </a>
            </div>
        </div>
    </div>
</section>

<!-- Featured Products -->
<?php if (class_exists('WooCommerce')): ?>
<section class="max-w-7xl mx-auto px-4 md:px-6 py-16 md:py-24">
    <div class="flex items-end justify-between mb-10">
        <div>
            <h2 class="text-2xl md:text-3xl font-heading font-bold">Featured Products</h2>
            <p class="text-slate-500 mt-2">Our most popular items</p>
        </div>
        <a href="<?php echo home_url('/shop'); ?>" class="text-blue-600 font-semibold text-sm hover:underline hidden md:block">View All &rarr;</a>
    </div>

    <div class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-6">
        <?php
        $products = wc_get_products(array('limit' => 8, 'status' => 'publish', 'orderby' => 'date'));
        foreach ($products as $product):
            $image = wp_get_attachment_image_src($product->get_image_id(), 'medium');
        ?>
        <div class="bg-white rounded-xl shadow-sm border border-slate-100 overflow-hidden group hover:shadow-md hover:scale-[1.02] transition-all duration-300">
            <div class="aspect-square bg-slate-50 overflow-hidden">
                <?php if ($image): ?>
                    <img src="<?php echo $image[0]; ?>" alt="<?php echo esc_attr($product->get_name()); ?>" class="w-full h-full object-cover group-hover:scale-110 transition-transform duration-500">
                <?php else: ?>
                    <div class="w-full h-full flex items-center justify-center text-slate-300">
                        <svg class="w-12 h-12" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="1" d="M4 16l4.586-4.586a2 2 0 012.828 0L16 16m-2-2l1.586-1.586a2 2 0 012.828 0L20 14m-6-6h.01M6 20h12a2 2 0 002-2V6a2 2 0 00-2-2H6a2 2 0 00-2 2v12a2 2 0 002 2z"/></svg>
                    </div>
                <?php endif; ?>
            </div>
            <div class="p-4">
                <h3 class="font-semibold text-sm mb-1 line-clamp-2"><?php echo $product->get_name(); ?></h3>
                <p class="text-blue-600 font-bold"><?php echo $product->get_price_html(); ?></p>
                <a href="<?php echo $product->get_permalink(); ?>" class="mt-3 block w-full bg-emerald-600 text-white text-center py-2 rounded-lg text-sm font-semibold hover:bg-emerald-700 transition">
                    Add to Cart
                </a>
            </div>
        </div>
        <?php endforeach; ?>
    </div>
</section>
<?php endif; ?>

<!-- Value Propositions -->
<section class="bg-white border-t border-slate-100">
    <div class="max-w-7xl mx-auto px-4 md:px-6 py-16 md:py-24">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-8">
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-blue-50 rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-blue-600" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 8h14M5 8a2 2 0 110-4h14a2 2 0 110 4M5 8v10a2 2 0 002 2h10a2 2 0 002-2V8m-9 4h4"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">Free Shipping</h3>
                <p class="text-slate-500 text-sm">Free delivery on all orders over a minimum spend</p>
            </div>
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-emerald-50 rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-emerald-600" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">Secure Payments</h3>
                <p class="text-slate-500 text-sm">100% secure payment processing</p>
            </div>
            <div class="text-center p-6">
                <div class="w-12 h-12 bg-amber-50 rounded-xl flex items-center justify-center mx-auto mb-4">
                    <svg class="w-6 h-6 text-amber-600" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M18.364 5.636l-3.536 3.536m0 5.656l3.536 3.536M9.172 9.172L5.636 5.636m3.536 9.192l-3.536 3.536M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-5 0a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
                </div>
                <h3 class="font-heading font-bold mb-2">24/7 Support</h3>
                <p class="text-slate-500 text-sm">Dedicated customer support around the clock</p>
            </div>
        </div>
    </div>
</section>

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
        <div class="prose prose-slate max-w-none">
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
            <time class="text-sm text-slate-500 font-medium"><?php echo get_the_date(); ?></time>
            <h1 class="text-3xl md:text-4xl font-heading font-bold mt-2"><?php the_title(); ?></h1>
        </header>
        <?php if (has_post_thumbnail()): ?>
        <div class="mb-8 rounded-xl overflow-hidden">
            <?php the_post_thumbnail('large', array('class' => 'w-full h-auto')); ?>
        </div>
        <?php endif; ?>
        <div class="prose prose-slate max-w-none">
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
file_put_contents($dir . "/single-product.php", <<<'PRODUCT'
<?php get_header(); ?>
<main class="max-w-7xl mx-auto px-4 md:px-6 py-12">
    <?php while (have_posts()) : the_post(); ?>
        <?php wc_get_template_part('content', 'single-product'); ?>
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
        <div class="w-16 h-1 bg-blue-600 rounded mt-3"></div>
    </header>
    <?php if (have_posts()): ?>
    <div class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-6">
        <?php while (have_posts()): the_post(); global $product; ?>
        <a href="<?php the_permalink(); ?>" class="bg-white rounded-xl shadow-sm border border-slate-100 hover:shadow-md hover:scale-[1.02] transition-all overflow-hidden flex flex-col">
            <div class="aspect-square bg-slate-100 overflow-hidden">
                <?php if (has_post_thumbnail()): ?>
                    <?php the_post_thumbnail('woocommerce_thumbnail', array('class' => 'w-full h-full object-cover')); ?>
                <?php endif; ?>
            </div>
            <div class="p-4 md:p-6 flex flex-col gap-2 flex-1">
                <h2 class="font-heading font-semibold text-slate-900"><?php the_title(); ?></h2>
                <?php if ($product instanceof WC_Product): ?>
                <div class="text-blue-600 font-bold mt-auto"><?php echo $product->get_price_html(); ?></div>
                <span class="bg-emerald-600 text-white text-center px-4 py-2 rounded-lg font-semibold text-sm hover:bg-emerald-700 transition">View Product</span>
                <?php endif; ?>
            </div>
        </a>
        <?php endwhile; ?>
    </div>
    <nav class="mt-10 flex justify-center gap-2 [&_a]:px-4 [&_a]:py-2 [&_a]:rounded-lg [&_a]:border [&_a]:border-slate-200 [&_span.current]:px-4 [&_span.current]:py-2 [&_span.current]:rounded-lg [&_span.current]:bg-blue-600 [&_span.current]:text-white">
        <?php woocommerce_pagination(); ?>
    </nav>
    <?php else: ?>
    <p class="text-slate-500">No products found.</p>
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
    <h1 class="text-6xl font-heading font-extrabold text-slate-200 mb-4">404</h1>
    <p class="text-xl text-slate-600 mb-8">Page not found</p>
    <a href="<?php echo home_url(); ?>" class="bg-blue-600 text-white px-6 py-3 rounded-lg font-semibold hover:bg-blue-700 transition">
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
    <article class="mb-12 pb-12 border-b border-slate-100 last:border-0">
        <h2 class="text-2xl font-heading font-bold mb-3">
            <a href="<?php the_permalink(); ?>" class="hover:text-blue-600 transition"><?php the_title(); ?></a>
        </h2>
        <time class="text-sm text-slate-500"><?php echo get_the_date(); ?></time>
        <div class="mt-4 text-slate-600 leading-relaxed">
            <?php the_excerpt(); ?>
        </div>
    </article>
    <?php endwhile; else : ?>
    <p class="text-slate-500">No content found.</p>
    <?php endif; ?>
</main>
<?php get_footer(); ?>
INDEX
);

echo "Theme '$name' created at themes/$slug with 9 template files + Tailwind CSS design system";
`
}
