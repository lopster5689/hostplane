package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/gin-gonic/gin"
)

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// maxOutputBytes caps WP-CLI output to prevent OOM on large results.
const maxOutputBytes = 1 << 20 // 1MB

// WPExecRequest is the structured request body for the wp-exec endpoint.
// Commands are never passed as raw strings — the control plane constructs
// the WP-CLI command from the tool name and validated parameters.
type WPExecRequest struct {
	Tool   string            `json:"tool" binding:"required"`
	Params map[string]string `json:"params"`
}

// WPExecResponse is the standard response from wp-exec.
type WPExecResponse struct {
	Site     string `json:"site"`
	Tool     string `json:"tool"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// safeOptionKeys restricts which wp_options can be read or modified via the agent.
// Dangerous keys (siteurl, home, admin_email, etc.) are excluded.
var safeOptionKeys = map[string]bool{
	"blogname":        true,
	"blogdescription": true,
	"date_format":     true,
	"time_format":     true,
	"timezone_string": true,
	"posts_per_page":  true,
	"show_on_front":   true,
	"page_on_front":   true,
	"page_for_posts":  true,
	// WooCommerce settings
	"woocommerce_coming_soon":       true,
	"woocommerce_shop_page_id":      true,
	"woocommerce_cart_page_id":      true,
	"woocommerce_checkout_page_id":  true,
	"woocommerce_myaccount_page_id": true,
	"woocommerce_currency":          true,
	"site_icon":                     true,
	"hostplane_home_sections":       true,
	"hostplane_contact":             true,
	"woocommerce_store_address":     true,
	"woocommerce_store_city":        true,
	"woocommerce_default_country":   true,
	"woocommerce_store_postcode":    true,
	"woocommerce_calc_taxes":        true,
	"woocommerce_enable_reviews":    true,
	// WooCommerce email branding
	"woocommerce_email_header_image":          true,
	"woocommerce_email_base_color":            true,
	"woocommerce_email_background_color":      true,
	"woocommerce_email_body_background_color": true,
	"woocommerce_email_text_color":            true,
	"woocommerce_email_footer_text":           true,
	// WooCommerce image sizes + display
	"woocommerce_thumbnail_cropping":       true,
	"woocommerce_single_image_width":       true,
	"woocommerce_thumbnail_image_width":    true,
	"woocommerce_category_archive_display": true,
	"woocommerce_default_catalog_orderby":  true,
	// WooCommerce payment gateways
	"woocommerce_cod_settings":    true,
	"woocommerce_bacs_settings":   true,
	"woocommerce_cheque_settings": true,
}

// validatePostID checks that the id param is a positive integer.
func validatePostID(id string) error {
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return fmt.Errorf("id must be a positive integer")
	}
	return nil
}

// wcPriceRe: a WooCommerce price through this API is a plain positive decimal
// with at most 2 places. Symbols ("₹120"), separators ("1,200"), and bare
// garbage are rejected HERE — a malformed price never reaches the database
// (the ÷100/"$1.20" storefront bug class, quality plan P2).
var wcPriceRe = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]{1,2})?$`)

func validateWCPrice(field, v string) error {
	if !wcPriceRe.MatchString(v) {
		return fmt.Errorf("invalid %s %q — must be a plain positive decimal like 120 or 120.50 (no currency symbols, no thousands separators)", field, v)
	}
	if f, err := strconv.ParseFloat(v, 64); err != nil || f <= 0 {
		return fmt.Errorf("invalid %s %q — must be greater than zero", field, v)
	}
	return nil
}

// menuURLRe: a custom menu link must be absolute http(s) with a real dotted
// host, or site-relative starting with "/". A bare word ("Shop") becomes
// <a href="http://Shop"> — the broken-nav bug. Internal pages should be added
// by post_id, which WordPress resolves itself.
var menuURLRe = regexp.MustCompile(`^(https?://[a-zA-Z0-9][a-zA-Z0-9.-]*\.[a-zA-Z]{2,}(:[0-9]+)?(/[^\s]*)?|/[^\s]*)$`)

// pluginSlugRe validates wordpress.org plugin slugs (lowercase alphanumeric + hyphens).
var pluginSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// themeSlugRe validates wordpress.org theme slugs.
var themeSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// themeFileAllowlist restricts which files can be written to a theme directory.
var themeFileAllowlist = map[string]bool{
	// Core WordPress templates
	"style.css":      true,
	"index.php":      true,
	"functions.php":  true,
	"header.php":     true,
	"footer.php":     true,
	"page.php":       true,
	"single.php":     true,
	"archive.php":    true,
	"search.php":     true,
	"404.php":        true,
	"sidebar.php":    true,
	"front-page.php": true,
	"home.php":       true,
	"comments.php":   true,
	// WooCommerce templates
	"woocommerce.php":                                   true,
	"single-product.php":                                true,
	"archive-product.php":                               true,
	"woocommerce/single-product.php":                    true,
	"woocommerce/archive-product.php":                   true,
	"woocommerce/cart/cart.php":                         true,
	"woocommerce/checkout/form-checkout.php":            true,
	"woocommerce/content-product.php":                   true,
	"woocommerce/content-single-product.php":            true,
	"woocommerce/single-product/title.php":              true,
	"woocommerce/single-product/price.php":              true,
	"woocommerce/single-product/short-description.php":  true,
	"woocommerce/single-product/add-to-cart/simple.php": true,
	"woocommerce/single-product/tabs/tabs.php":          true,
	"woocommerce/single-product/product-image.php":      true,
	"woocommerce/loop/result-count.php":                 true,
	"woocommerce/loop/orderby.php":                      true,
	"woocommerce/loop/price.php":                        true,
	"woocommerce/loop/add-to-cart.php":                  true,
	// Template parts
	"template-parts/header.php":  true,
	"template-parts/footer.php":  true,
	"template-parts/content.php": true,
	"template-parts/sidebar.php": true,
}

// themeSlugSafeRe validates custom theme slugs (more permissive than wordpress.org slugs).
var themeSlugSafeRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// postFieldAllowlist restricts which post fields can be updated.
var postFieldAllowlist = map[string]bool{
	"post_title":   true,
	"post_content": true,
	"post_excerpt": true,
	"post_status":  true,
	"post_date":    true,
}

// commentActionAllowlist restricts valid comment moderation actions.
var commentActionAllowlist = map[string]bool{
	"approve":   true,
	"unapprove": true,
	"spam":      true,
	"trash":     true,
}

// safePostMetaKeys restricts which post meta keys can be read/written via the agent.
var safePostMetaKeys = map[string]bool{
	"_thumbnail_id":     true,
	"_wp_page_template": true,
	// Marks attachments imported as free brand placeholders, so the paid
	// photography pass can tell which slots still need real images.
	"_hostplane_placeholder": true,
	// WooCommerce product meta
	"_product_image_gallery": true,
	"_regular_price":         true,
	"_sale_price":            true,
	"_sku":                   true,
	"_stock":                 true,
	"_stock_status":          true,
	"_manage_stock":          true,
	"_weight":                true,
	"_length":                true,
	"_width":                 true,
	"_height":                true,
	"_virtual":               true,
	"_downloadable":          true,
	// SEO plugin meta (Yoast + RankMath)
	"_yoast_wpseo_title":      true,
	"_yoast_wpseo_metadesc":   true,
	"rank_math_title":         true,
	"rank_math_description":   true,
	"rank_math_focus_keyword": true,
}

// permalinkStructures restricts valid permalink patterns.
var permalinkStructures = map[string]bool{
	"/%postname%/":                         true,
	"/%year%/%monthnum%/%day%/%postname%/": true,
	"/%year%/%monthnum%/%postname%/":       true,
	"/%category%/%postname%/":              true,
	"/archives/%post_id%":                  true,
}

// wcProductFields restricts which product fields can be updated via wp wc product update.
var wcProductFields = map[string]bool{
	"name":               true,
	"regular_price":      true,
	"sale_price":         true,
	"description":        true,
	"short_description":  true,
	"sku":                true,
	"manage_stock":       true,
	"stock_quantity":     true,
	"stock_status":       true,
	"status":             true,
	"catalog_visibility": true,
	"weight":             true,
	"virtual":            true,
	"downloadable":       true,
}

// wcOrderStatuses restricts valid order status transitions.
var wcOrderStatuses = map[string]bool{
	"pending":    true,
	"processing": true,
	"on-hold":    true,
	"completed":  true,
	"cancelled":  true,
	"refunded":   true,
}

// wcCouponTypes restricts valid coupon discount types.
var wcCouponTypes = map[string]bool{
	"percent":       true,
	"fixed_cart":    true,
	"fixed_product": true,
}

// couponCodeRe validates coupon codes (alphanumeric, hyphens, underscores).
var couponCodeRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// buildTool maps a tool name + params to a safe WP-CLI argument array.
// Returns the []string to pass to ContainerExecCreate.Cmd.
func buildTool(tool string, params map[string]string) ([]string, error) {
	switch tool {
	case "get_site_info":
		return []string{"wp", "option", "get", "blogname", "--format=json", "--allow-root"}, nil

	case "get_site_overview":
		phpCode := `
			$data = array(
				'site_name' => get_bloginfo('name'),
				'tagline' => get_bloginfo('description'),
				'url' => home_url(),
				'active_theme' => get_stylesheet(),
				'show_on_front' => get_option('show_on_front'),
				'page_on_front' => get_option('page_on_front'),
				'posts_per_page' => get_option('posts_per_page'),
				'timezone' => get_option('timezone_string'),
				'permalink_structure' => get_option('permalink_structure'),
			);
			$plugins = get_option('active_plugins', array());
			$data['active_plugins'] = array_map(function($p) { return explode('/', $p)[0]; }, $plugins);
			$pages = get_posts(array('post_type' => 'page', 'post_status' => 'any', 'numberposts' => 50, 'fields' => 'ids'));
			$data['pages'] = array_map(function($id) { return array('id' => $id, 'title' => get_the_title($id), 'status' => get_post_status($id)); }, $pages);
			if (class_exists('WooCommerce')) {
				$data['woocommerce'] = true;
				$data['product_count'] = wp_count_posts('product')->publish;
				$data['order_count'] = wp_count_posts('shop_order')->{'wc-processing'} + wp_count_posts('shop_order')->{'wc-completed'};
				$data['currency'] = get_woocommerce_currency();
				$data['coming_soon'] = get_option('woocommerce_coming_soon', 'no');
			} else {
				$data['woocommerce'] = false;
			}
			echo json_encode($data);
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "get_option":
		key := params["key"]
		if key == "" {
			return nil, fmt.Errorf("key is required")
		}
		if !safeOptionKeys[key] {
			return nil, fmt.Errorf("option key %q is not allowed", key)
		}
		return []string{"wp", "option", "get", key, "--format=json", "--allow-root"}, nil

	case "update_option":
		key := params["key"]
		value := params["value"]
		if key == "" || value == "" {
			return nil, fmt.Errorf("key and value are required")
		}
		if !safeOptionKeys[key] {
			return nil, fmt.Errorf("option key %q is not allowed", key)
		}
		// Structured options are read-open but write-closed: raw writes would
		// bypass their validators (set_home_sections / contact mirroring).
		if key == "hostplane_home_sections" || key == "hostplane_contact" {
			return nil, fmt.Errorf("option %q is read-only here — use set_home_sections", key)
		}
		// A draft front page 404s the homepage (bit a real user 2026-07-09):
		// assigning it as the front page IS the intent to publish it.
		if key == "page_on_front" {
			if err := validatePostID(value); err != nil {
				return nil, fmt.Errorf("page_on_front must be a page ID: %w", err)
			}
			php := `$pid = ` + value + `; $p = get_post($pid);
				if (!$p || $p->post_type !== 'page') { echo 'page_on_front: ' . $pid . ' is not a page'; exit(1); }
				if ($p->post_status !== 'publish') { wp_update_post(array('ID' => $pid, 'post_status' => 'publish')); }
				update_option('page_on_front', $pid); update_option('show_on_front', 'page');
				echo 'front page set to ' . $pid . ' (published)';`
			return []string{"wp", "eval", php, "--allow-root"}, nil
		}
		return []string{"wp", "option", "update", key, value, "--allow-root"}, nil

	case "list_posts":
		postType := params["post_type"]
		if postType == "" {
			postType = "post"
		}
		if postType != "post" && postType != "page" && postType != "product" {
			return nil, fmt.Errorf("post_type must be 'post', 'page', or 'product'")
		}
		cmd := []string{"wp", "post", "list", "--post_type=" + postType, "--format=json", "--allow-root"}
		if status := params["status"]; status != "" {
			if status != "publish" && status != "draft" && status != "any" {
				return nil, fmt.Errorf("status must be 'publish', 'draft', or 'any'")
			}
			cmd = append(cmd, "--post_status="+status)
		}
		return cmd, nil

	case "get_post":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "post", "get", id, "--format=json", "--allow-root"}, nil

	case "update_post":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		field := params["field"]
		value := params["value"]
		if field == "" || value == "" {
			return nil, fmt.Errorf("field and value are required")
		}
		if !postFieldAllowlist[field] {
			return nil, fmt.Errorf("field %q is not allowed", field)
		}
		return []string{"wp", "post", "update", id, "--" + field + "=" + value, "--user=1", "--allow-root"}, nil

	case "create_post":
		postType := params["post_type"]
		title := params["post_title"]
		if postType == "" {
			postType = "page"
		}
		if postType != "post" && postType != "page" && postType != "product" {
			return nil, fmt.Errorf("post_type must be 'post', 'page', or 'product'")
		}
		if title == "" {
			return nil, fmt.Errorf("post_title is required")
		}
		cmd := []string{"wp", "post", "create", "--post_type=" + postType, "--post_title=" + title, "--post_status=draft", "--user=1", "--allow-root"}
		if content := params["post_content"]; content != "" {
			cmd = append(cmd, "--post_content="+content)
		}
		return cmd, nil

	case "list_plugins":
		return []string{"wp", "plugin", "list", "--format=json", "--allow-root"}, nil

	case "install_plugin":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !pluginSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid plugin slug format")
		}
		return []string{"wp", "plugin", "install", slug, "--activate", "--allow-root"}, nil

	case "deactivate_plugin":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !pluginSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid plugin slug format")
		}
		return []string{"wp", "plugin", "deactivate", slug, "--allow-root"}, nil

	case "list_themes":
		return []string{"wp", "theme", "list", "--format=json", "--allow-root"}, nil

	case "install_theme":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !themeSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid theme slug format")
		}
		return []string{"wp", "theme", "install", slug, "--allow-root"}, nil

	case "activate_theme":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !themeSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid theme slug format")
		}
		return []string{"wp", "theme", "activate", slug, "--allow-root"}, nil

	case "delete_theme":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !themeSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid theme slug format")
		}
		return []string{"wp", "theme", "delete", slug, "--allow-root"}, nil

	// ── Theme customization ──────────────────────────────────────
	case "get_theme_mods":
		return []string{"wp", "theme", "mod", "list", "--format=json", "--allow-root"}, nil

	case "set_theme_mod":
		key := params["key"]
		value := params["value"]
		if key == "" || value == "" {
			return nil, fmt.Errorf("key and value are required")
		}
		return []string{"wp", "theme", "mod", "set", key, value, "--allow-root"}, nil

	case "get_custom_css":
		phpCode := `echo wp_get_custom_css();`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "set_custom_css":
		css := params["css"]
		if css == "" {
			return nil, fmt.Errorf("css is required")
		}
		encoded := base64Encode(css)
		phpCode := `$css = base64_decode("` + encoded + `"); wp_update_custom_css_post($css); echo "Custom CSS updated";`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "append_custom_css":
		css := params["css"]
		if css == "" {
			return nil, fmt.Errorf("css is required")
		}
		encoded := base64Encode(css)
		phpCode := `$new = base64_decode("` + encoded + `"); $existing = wp_get_custom_css(); wp_update_custom_css_post($existing . "\n" . $new); echo "CSS appended";`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	// ── Custom theme creation & editing ─────────────────────────
	// NOTE: create_theme is handled as a special case in handleWPExec (uses Docker stdin for large PHP script)
	case "create_theme":
		return nil, fmt.Errorf("create_theme is handled separately")

	case "_create_theme_old": // dead code — kept for reference
		name := params["name"]
		slug := params["slug"]
		if name == "" || slug == "" {
			return nil, fmt.Errorf("name and slug are required")
		}
		if !themeSlugSafeRe.MatchString(slug) {
			return nil, fmt.Errorf("slug must be lowercase alphanumeric with hyphens")
		}
		description := params["description"]
		if description == "" {
			description = "Custom theme created by Hostplane AI Editor"
		}
		// Create theme directory and minimal required files via wp eval.
		// style.css (with WordPress theme header) + index.php + functions.php (with Tailwind CDN)
		encodedName := base64Encode(name)
		encodedDesc := base64Encode(description)
		encodedSlug := base64Encode(slug)
		phpCode := `
			$slug = base64_decode("` + encodedSlug + `");
			$name = base64_decode("` + encodedName + `");
			$desc = base64_decode("` + encodedDesc + `");
			$prefix = str_replace("-", "_", $slug);
			$dir = WP_CONTENT_DIR . "/themes/" . $slug;
			if (is_dir($dir)) { echo "Error: theme already exists"; exit(1); }
			mkdir($dir, 0755, true);
			mkdir($dir . "/template-parts", 0755, true);

			// style.css with WordPress theme header
			file_put_contents($dir . "/style.css", "/*\nTheme Name: $name\nDescription: $desc\nVersion: 1.0\n*/\n");

			// functions.php with Tailwind CDN
			$functions = "<?php\n" .
				"function {$prefix}_enqueue() {\n" .
				"  wp_enqueue_script('tailwindcss', 'https://cdn.tailwindcss.com', array(), null);\n" .
				"  wp_enqueue_style('{$prefix}-style', get_stylesheet_uri());\n" .
				"}\n" .
				"add_action('wp_enqueue_scripts', '{$prefix}_enqueue');\n\n" .
				"// WooCommerce support\n" .
				"function {$prefix}_setup() {\n" .
				"  add_theme_support('title-tag');\n" .
				"  add_theme_support('post-thumbnails');\n" .
				"  add_theme_support('woocommerce');\n" .
				"  add_theme_support('custom-logo');\n" .
				"  register_nav_menus(array('primary' => 'Primary Menu', 'footer' => 'Footer Menu'));\n" .
				"}\n" .
				"add_action('after_setup_theme', '{$prefix}_setup');\n";
			file_put_contents($dir . "/functions.php", $functions);

			// Minimal index.php
			$index = "<?php get_header(); ?>\n" .
				"<main class=\"container mx-auto px-4 py-8\">\n" .
				"  <?php if (have_posts()) : while (have_posts()) : the_post(); ?>\n" .
				"    <article class=\"mb-8\">\n" .
				"      <h2 class=\"text-2xl font-bold mb-2\"><a href=\"<?php the_permalink(); ?>\"><?php the_title(); ?></a></h2>\n" .
				"      <div class=\"prose\"><?php the_content(); ?></div>\n" .
				"    </article>\n" .
				"  <?php endwhile; else : ?>\n" .
				"    <p>No content found.</p>\n" .
				"  <?php endif; ?>\n" .
				"</main>\n" .
				"<?php get_footer(); ?>\n";
			file_put_contents($dir . "/index.php", $index);

			// header.php
			$header = "<!DOCTYPE html>\n<html <?php language_attributes(); ?>>\n<head>\n" .
				"  <meta charset=\"<?php bloginfo('charset'); ?>\">\n" .
				"  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n" .
				"  <?php wp_head(); ?>\n" .
				"</head>\n<body <?php body_class('bg-white text-gray-900'); ?>>\n" .
				"<?php wp_body_open(); ?>\n" .
				"<header class=\"bg-white shadow\">\n" .
				"  <div class=\"container mx-auto px-4 py-4 flex items-center justify-between\">\n" .
				"    <a href=\"<?php echo home_url(); ?>\" class=\"text-xl font-bold\"><?php bloginfo('name'); ?></a>\n" .
				"    <nav><?php wp_nav_menu(array('theme_location' => 'primary', 'container' => false, 'menu_class' => 'flex space-x-6')); ?></nav>\n" .
				"  </div>\n" .
				"</header>\n";
			file_put_contents($dir . "/header.php", $header);

			// footer.php
			$footer = "<footer class=\"bg-gray-100 mt-16\">\n" .
				"  <div class=\"container mx-auto px-4 py-8 text-center text-gray-600\">\n" .
				"    <p>&copy; <?php echo date('Y'); ?> <?php bloginfo('name'); ?></p>\n" .
				"  </div>\n" .
				"</footer>\n<?php wp_footer(); ?>\n</body>\n</html>\n";
			file_put_contents($dir . "/footer.php", $footer);

			echo "Theme '$name' created at themes/$slug with Tailwind CSS";
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "write_theme_file":
		filename := params["filename"]
		content := params["content"]
		if filename == "" || content == "" {
			return nil, fmt.Errorf("filename and content are required")
		}
		if !themeFileAllowlist[filename] {
			return nil, fmt.Errorf("filename %q is not allowed; allowed: style.css, index.php, header.php, footer.php, page.php, single.php, archive.php, search.php, 404.php, sidebar.php, front-page.php, home.php, comments.php, woocommerce.php, functions.php, template-parts/*.php", filename)
		}
		// Size cap: 64KB per file
		if len(content) > 65536 {
			return nil, fmt.Errorf("content too large (max 64KB)")
		}
		encoded := base64Encode(content)
		phpCode := `
			$theme_dir = get_stylesheet_directory();
			$filename = "` + filename + `";
			$content = base64_decode("` + encoded + `");
			$path = $theme_dir . "/" . $filename;
			$dir = dirname($path);
			if (!is_dir($dir)) { mkdir($dir, 0755, true); }
			file_put_contents($path, $content);
			echo "Written " . strlen($content) . " bytes to " . $filename;
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "read_theme_file":
		filename := params["filename"]
		if filename == "" {
			return nil, fmt.Errorf("filename is required")
		}
		if !themeFileAllowlist[filename] {
			return nil, fmt.Errorf("filename %q is not allowed", filename)
		}
		phpCode := `
			$path = get_stylesheet_directory() . "/` + filename + `";
			if (!file_exists($path)) { echo "File not found"; exit(1); }
			echo file_get_contents($path);
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "list_theme_files":
		phpCode := `
			$dir = get_stylesheet_directory();
			$files = array();
			$iterator = new RecursiveIteratorIterator(new RecursiveDirectoryIterator($dir, RecursiveDirectoryIterator::SKIP_DOTS));
			foreach ($iterator as $file) {
				if ($file->isFile()) {
					$files[] = array(
						'file' => str_replace($dir . '/', '', $file->getPathname()),
						'size' => $file->getSize(),
					);
				}
			}
			echo json_encode($files);
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	// ── Sidebar & Widget management ──────────────────────────────
	case "list_sidebars":
		return []string{"wp", "sidebar", "list", "--format=json", "--allow-root"}, nil

	case "list_widgets":
		sidebar := params["sidebar"]
		if sidebar == "" {
			return nil, fmt.Errorf("sidebar is required")
		}
		return []string{"wp", "widget", "list", sidebar, "--format=json", "--allow-root"}, nil

	case "add_widget":
		widgetType := params["type"]
		sidebar := params["sidebar"]
		if widgetType == "" || sidebar == "" {
			return nil, fmt.Errorf("type and sidebar are required")
		}
		cmd := []string{"wp", "widget", "add", widgetType, sidebar, "--allow-root"}
		if title := params["title"]; title != "" {
			cmd = append(cmd, "--title="+title)
		}
		if text := params["text"]; text != "" {
			cmd = append(cmd, "--text="+text)
		}
		if url := params["url"]; url != "" {
			cmd = append(cmd, "--url="+url)
		}
		return cmd, nil

	case "delete_widget":
		widgetID := params["widget_id"]
		if widgetID == "" {
			return nil, fmt.Errorf("widget_id is required")
		}
		return []string{"wp", "widget", "delete", widgetID, "--allow-root"}, nil

	// ── Menu location assignment ─────────────────────────────────
	case "list_menu_locations":
		return []string{"wp", "menu", "location", "list", "--format=json", "--allow-root"}, nil

	case "assign_menu_location":
		menu := params["menu"]
		location := params["location"]
		if menu == "" || location == "" {
			return nil, fmt.Errorf("menu and location are required")
		}
		return []string{"wp", "menu", "location", "assign", menu, location, "--allow-root"}, nil

	// ── WooCommerce: Sample data ─────────────────────────────────
	case "import_sample_data":
		// WooCommerce bundles sample data as a CSV. Use wp eval to trigger the importer.
		phpCode := `
			if (!class_exists('WC_Install')) { echo 'WooCommerce not active'; exit(1); }
			include_once WC_ABSPATH . 'includes/admin/importers/class-wc-product-csv-importer-controller.php';
			$file = WC_ABSPATH . 'sample-data/sample_products.csv';
			if (!file_exists($file)) { echo 'Sample data file not found'; exit(1); }
			$params = array('delimiter' => ',', 'mapping' => array(), 'update_existing' => false);
			$importer = new WC_Product_CSV_Importer($file, $params);
			$results = $importer->import();
			$imported = count($results['imported']);
			$failed = count($results['failed']);
			echo "Imported $imported products ($failed failed)";
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "search_replace":
		old := params["old"]
		new := params["new"]
		if old == "" || new == "" {
			return nil, fmt.Errorf("old and new are required")
		}
		// MVP: search_replace is dry-run only (read-only preview of what would change).
		// Live execution requires pre-backup integration (V2).
		cmd := []string{"wp", "search-replace", old, new, "--dry-run", "--allow-root"}
		return cmd, nil

	case "media_import":
		url := params["url"]
		if url == "" {
			return nil, fmt.Errorf("url is required")
		}
		if !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("url must start with https://")
		}
		// Block SSRF to internal networks
		for _, blocked := range []string{
			"://127.", "://10.", "://192.168.", "://172.16.", "://172.17.",
			"://172.18.", "://172.19.", "://172.20.", "://172.21.", "://172.22.",
			"://172.23.", "://172.24.", "://172.25.", "://172.26.", "://172.27.",
			"://172.28.", "://172.29.", "://172.30.", "://172.31.",
			"://169.254.", "://[::1]", "://localhost",
		} {
			if strings.Contains(url, blocked) {
				return nil, fmt.Errorf("url must point to a public host")
			}
		}
		cmd := []string{"wp", "media", "import", url, "--allow-root"}
		if title := params["title"]; title != "" {
			cmd = append(cmd, "--title="+title)
		}
		return cmd, nil

	// ── Delete post ──────────────────────────────────────────────
	case "delete_post":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		// Moves to trash by default (safe); --force would permanently delete
		return []string{"wp", "post", "delete", id, "--allow-root"}, nil

	// ── Menu management ──────────────────────────────────────────
	case "list_menus":
		return []string{"wp", "menu", "list", "--format=json", "--allow-root"}, nil

	case "create_menu":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		return []string{"wp", "menu", "create", name, "--allow-root"}, nil

	case "add_menu_item":
		menuID := params["menu_id"]
		if menuID == "" {
			return nil, fmt.Errorf("menu_id is required")
		}
		if err := validatePostID(menuID); err != nil {
			return nil, fmt.Errorf("invalid menu_id: %w", err)
		}
		// Support adding posts/pages or custom URLs
		if postID := params["post_id"]; postID != "" {
			if err := validatePostID(postID); err != nil {
				return nil, fmt.Errorf("invalid post_id: %w", err)
			}
			return []string{"wp", "menu", "item", "add-post", menuID, postID, "--allow-root"}, nil
		}
		if url := params["url"]; url != "" {
			title := params["title"]
			if title == "" {
				return nil, fmt.Errorf("title is required for custom URL menu items")
			}
			if !menuURLRe.MatchString(url) {
				return nil, fmt.Errorf("invalid menu url %q — for pages/posts on this site pass post_id instead; a custom url must be absolute (https://example.com/...) or site-relative starting with /", url)
			}
			// wp-cli signature: add-custom <menu> <title> <link> — title FIRST.
			// (Was swapped; produced items titled "/" linking to "http://Home".)
			return []string{"wp", "menu", "item", "add-custom", menuID, title, url, "--allow-root"}, nil
		}
		return nil, fmt.Errorf("either post_id or url+title is required")

	// ── Comment management ───────────────────────────────────────
	case "list_comments":
		cmd := []string{"wp", "comment", "list", "--format=json", "--allow-root"}
		if status := params["status"]; status != "" {
			if status != "approve" && status != "hold" && status != "spam" && status != "trash" && status != "all" {
				return nil, fmt.Errorf("status must be 'approve', 'hold', 'spam', 'trash', or 'all'")
			}
			cmd = append(cmd, "--status="+status)
		}
		if number := params["number"]; number != "" {
			n, err := strconv.Atoi(number)
			if err != nil || n <= 0 || n > 100 {
				return nil, fmt.Errorf("number must be 1-100")
			}
			cmd = append(cmd, "--number="+number)
		}
		return cmd, nil

	case "moderate_comment":
		id := params["id"]
		action := params["action"]
		if id == "" || action == "" {
			return nil, fmt.Errorf("id and action are required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		if !commentActionAllowlist[action] {
			return nil, fmt.Errorf("action must be 'approve', 'unapprove', 'spam', or 'trash'")
		}
		return []string{"wp", "comment", action, id, "--allow-root"}, nil

	case "delete_comment":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "comment", "delete", id, "--force", "--allow-root"}, nil

	// ── Taxonomy (categories/tags) ───────────────────────────────
	case "list_categories":
		return []string{"wp", "term", "list", "category", "--format=json", "--allow-root"}, nil

	case "create_category":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		cmd := []string{"wp", "term", "create", "category", name, "--allow-root"}
		if parent := params["parent"]; parent != "" {
			if err := validatePostID(parent); err != nil {
				return nil, fmt.Errorf("invalid parent: %w", err)
			}
			cmd = append(cmd, "--parent="+parent)
		}
		return cmd, nil

	case "list_tags":
		return []string{"wp", "term", "list", "post_tag", "--format=json", "--allow-root"}, nil

	case "create_tag":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		return []string{"wp", "term", "create", "post_tag", name, "--allow-root"}, nil

	case "set_post_terms":
		id := params["id"]
		taxonomy := params["taxonomy"]
		terms := params["terms"]
		if id == "" || taxonomy == "" || terms == "" {
			return nil, fmt.Errorf("id, taxonomy, and terms are required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		if taxonomy != "category" && taxonomy != "post_tag" && taxonomy != "product_cat" && taxonomy != "product_tag" {
			return nil, fmt.Errorf("taxonomy must be 'category', 'post_tag', 'product_cat', or 'product_tag'")
		}
		// terms is a comma-separated list of term IDs or slugs
		return []string{"wp", "post", "term", "set", id, taxonomy, terms, "--allow-root"}, nil

	// ── Discovery (search plugins/themes) ────────────────────────
	case "search_plugins":
		query := params["query"]
		if query == "" {
			return nil, fmt.Errorf("query is required")
		}
		return []string{"wp", "plugin", "search", query, "--fields=name,slug,rating,active_installs", "--format=json", "--allow-root"}, nil

	case "search_themes":
		query := params["query"]
		if query == "" {
			return nil, fmt.Errorf("query is required")
		}
		return []string{"wp", "theme", "search", query, "--fields=name,slug,rating", "--format=json", "--allow-root"}, nil

	// ── Post meta ────────────────────────────────────────────────
	case "get_post_meta":
		id := params["id"]
		key := params["key"]
		if id == "" || key == "" {
			return nil, fmt.Errorf("id and key are required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		if !safePostMetaKeys[key] {
			return nil, fmt.Errorf("meta key %q is not allowed", key)
		}
		// Use wp eval instead of wp post meta get — the latter exits 1 when key doesn't exist
		encodedKey := base64Encode(key)
		phpCode := `$v = get_post_meta(` + id + `, base64_decode("` + encodedKey + `"), true); echo $v === "" ? "(empty)" : $v;`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "set_post_meta":
		id := params["id"]
		key := params["key"]
		value := params["value"]
		if id == "" || key == "" || value == "" {
			return nil, fmt.Errorf("id, key, and value are required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		if !safePostMetaKeys[key] {
			return nil, fmt.Errorf("meta key %q is not allowed", key)
		}
		return []string{"wp", "post", "meta", "update", id, key, value, "--allow-root"}, nil

	// ── Permalink structure ──────────────────────────────────────
	case "update_permalink_structure":
		structure := params["structure"]
		if structure == "" {
			return nil, fmt.Errorf("structure is required")
		}
		if !permalinkStructures[structure] {
			return nil, fmt.Errorf("structure %q is not allowed; use one of: /%%postname%%/, /%%year%%/%%monthnum%%/%%postname%%/, etc.", structure)
		}
		// Note: this sets the structure. WP auto-flushes on next request.
		return []string{"wp", "rewrite", "structure", structure, "--allow-root"}, nil

	// ── WooCommerce: Products ────────────────────────────────────
	case "set_product_attributes":
		// Structured product specs (GSM, Material, Fit, Pesticide-free…) from
		// the CSV product shape — written as real WooCommerce attributes so
		// the product page can render them as a spec table.
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		attrsJSON := params["attributes"]
		if attrsJSON == "" {
			return nil, fmt.Errorf("attributes is required: JSON array of {\"label\",\"value\"}")
		}
		var attrs []struct{ Label, Value string }
		if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
			var alt []map[string]string
			if err2 := json.Unmarshal([]byte(attrsJSON), &alt); err2 != nil {
				return nil, fmt.Errorf("attributes must be a JSON array of {\"label\",\"value\"}: %v", err)
			}
			for _, m := range alt {
				attrs = append(attrs, struct{ Label, Value string }{m["label"], m["value"]})
			}
		}
		if len(attrs) == 0 || len(attrs) > 12 {
			return nil, fmt.Errorf("attributes must contain 1..12 entries")
		}
		clean := make([][2]string, 0, len(attrs))
		for _, a := range attrs {
			label := strings.TrimSpace(a.Label)
			value := strings.TrimSpace(a.Value)
			if label == "" || value == "" {
				continue
			}
			if len(label) > 60 {
				label = label[:60]
			}
			if len(value) > 160 {
				value = value[:160]
			}
			clean = append(clean, [2]string{label, value})
		}
		if len(clean) == 0 {
			return nil, fmt.Errorf("no valid attribute entries after trimming")
		}
		payload, _ := json.Marshal(clean)
		payloadB64 := base64.StdEncoding.EncodeToString(payload)
		phpAttrs := `
			$pid = ` + id + `;
			$product = function_exists('wc_get_product') ? wc_get_product($pid) : null;
			if (!$product) { echo 'not a product: ' . $pid; exit(1); }
			$pairs = json_decode(base64_decode('` + payloadB64 + `'), true);
			$attrs = array();
			$pos = 0;
			foreach ($pairs as $pair) {
				$a = new WC_Product_Attribute();
				$a->set_name(wp_strip_all_tags($pair[0]));
				$a->set_options(array(wp_strip_all_tags($pair[1])));
				$a->set_position($pos++);
				$a->set_visible(true);
				$a->set_variation(false);
				$attrs[] = $a;
			}
			$product->set_attributes($attrs);
			$product->save();
			echo 'attributes set: ' . count($attrs);
		`
		return []string{"wp", "eval", phpAttrs, "--allow-root"}, nil

	case "media_pass_state":
		// Read-only, single round-trip snapshot for the photography pass:
		// home sections, site icon, products with thumbnails, and which
		// attachments are tagged placeholders. Replaces ~15 sequential reads
		// that blew past Cloudflare's 100s proxy timeout on the quote path.
		phpState := `
			$out = array();
			$out["sections"] = json_decode(get_option("hostplane_home_sections", "[]"), true);
			if (!is_array($out["sections"])) { $out["sections"] = array(); }
			$out["site_icon"] = (string) get_option("site_icon", "");
			$out["products"] = array();
			foreach (get_posts(array("post_type"=>"product","numberposts"=>-1,"post_status"=>"publish")) as $p) {
				$out["products"][] = array(
					"id" => (string) $p->ID,
					"name" => $p->post_title,
					"thumb" => (string) get_post_meta($p->ID, "_thumbnail_id", true),
				);
			}
			$out["placeholders"] = array_map("strval", get_posts(array(
				"post_type"=>"attachment","numberposts"=>-1,"post_status"=>"any",
				"meta_key"=>"_hostplane_placeholder","meta_value"=>"1","fields"=>"ids")));
			echo json_encode($out);
		`
		return []string{"wp", "eval", phpState, "--allow-root"}, nil

	case "list_products":
		cmd := []string{"wp", "wc", "product", "list", "--user=1", "--format=json", "--allow-root"}
		if status := params["status"]; status != "" {
			if status != "publish" && status != "draft" && status != "pending" && status != "any" {
				return nil, fmt.Errorf("status must be 'publish', 'draft', 'pending', or 'any'")
			}
			cmd = append(cmd, "--status="+status)
		}
		return cmd, nil

	case "get_product":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "wc", "product", "get", id, "--user=1", "--format=json", "--allow-root"}, nil

	case "create_product":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		productType := params["type"]
		if productType == "" {
			productType = "simple"
		}
		if productType != "simple" && productType != "grouped" && productType != "external" && productType != "variable" {
			return nil, fmt.Errorf("type must be 'simple', 'grouped', 'external', or 'variable'")
		}
		cmd := []string{"wp", "wc", "product", "create", "--name=" + name, "--type=" + productType, "--user=1", "--porcelain", "--allow-root"}
		if price := params["regular_price"]; price != "" {
			if err := validateWCPrice("regular_price", price); err != nil {
				return nil, err
			}
			cmd = append(cmd, "--regular_price="+price)
		}
		if salePrice := params["sale_price"]; salePrice != "" {
			if err := validateWCPrice("sale_price", salePrice); err != nil {
				return nil, err
			}
			cmd = append(cmd, "--sale_price="+salePrice)
		}
		if desc := params["description"]; desc != "" {
			cmd = append(cmd, "--description="+desc)
		}
		if shortDesc := params["short_description"]; shortDesc != "" {
			cmd = append(cmd, "--short_description="+shortDesc)
		}
		if sku := params["sku"]; sku != "" {
			cmd = append(cmd, "--sku="+sku)
		}
		if status := params["status"]; status != "" {
			if status != "publish" && status != "draft" && status != "pending" {
				return nil, fmt.Errorf("status must be 'publish', 'draft', or 'pending'")
			}
			cmd = append(cmd, "--status="+status)
		}
		if stock := params["stock_quantity"]; stock != "" {
			cmd = append(cmd, "--manage_stock=true", "--stock_quantity="+stock)
		}
		if virtual := params["virtual"]; virtual == "true" {
			cmd = append(cmd, "--virtual=true")
		}
		return cmd, nil

	case "update_product":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		field := params["field"]
		value := params["value"]
		if field == "" || value == "" {
			return nil, fmt.Errorf("field and value are required")
		}
		if !wcProductFields[field] {
			return nil, fmt.Errorf("field %q is not allowed", field)
		}
		if field == "regular_price" || field == "sale_price" {
			if err := validateWCPrice(field, value); err != nil {
				return nil, err
			}
		}
		return []string{"wp", "wc", "product", "update", id, "--" + field + "=" + value, "--user=1", "--allow-root"}, nil

	case "delete_product":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		// Moves to trash (no --force, so it's recoverable)
		return []string{"wp", "wc", "product", "delete", id, "--user=1", "--allow-root"}, nil

	// ── WooCommerce: Product Categories ──────────────────────────
	case "list_product_categories":
		return []string{"wp", "wc", "product_cat", "list", "--user=1", "--format=json", "--allow-root"}, nil

	case "create_product_category":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		cmd := []string{"wp", "wc", "product_cat", "create", "--name=" + name, "--user=1", "--porcelain", "--allow-root"}
		if desc := params["description"]; desc != "" {
			cmd = append(cmd, "--description="+desc)
		}
		return cmd, nil

	// ── WooCommerce: Orders ──────────────────────────────────────
	case "list_orders":
		cmd := []string{"wp", "wc", "shop_order", "list", "--user=1", "--format=json", "--allow-root"}
		if status := params["status"]; status != "" {
			if !wcOrderStatuses[status] && status != "any" {
				return nil, fmt.Errorf("invalid order status")
			}
			cmd = append(cmd, "--status="+status)
		}
		return cmd, nil

	case "get_order":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "wc", "shop_order", "get", id, "--user=1", "--format=json", "--allow-root"}, nil

	case "update_order_status":
		id := params["id"]
		status := params["status"]
		if id == "" || status == "" {
			return nil, fmt.Errorf("id and status are required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		if !wcOrderStatuses[status] {
			return nil, fmt.Errorf("status must be one of: pending, processing, on-hold, completed, cancelled, refunded")
		}
		return []string{"wp", "wc", "shop_order", "update", id, "--status=" + status, "--user=1", "--allow-root"}, nil

	// ── WooCommerce: Coupons ─────────────────────────────────────
	case "list_coupons":
		return []string{"wp", "wc", "shop_coupon", "list", "--user=1", "--format=json", "--allow-root"}, nil

	case "create_coupon":
		code := params["code"]
		discountType := params["discount_type"]
		amount := params["amount"]
		if code == "" || discountType == "" || amount == "" {
			return nil, fmt.Errorf("code, discount_type, and amount are required")
		}
		if !couponCodeRe.MatchString(code) {
			return nil, fmt.Errorf("coupon code must be alphanumeric (hyphens and underscores allowed)")
		}
		if !wcCouponTypes[discountType] {
			return nil, fmt.Errorf("discount_type must be 'percent', 'fixed_cart', or 'fixed_product'")
		}
		cmd := []string{"wp", "wc", "shop_coupon", "create", "--code=" + code, "--discount_type=" + discountType, "--amount=" + amount, "--user=1", "--porcelain", "--allow-root"}
		if desc := params["description"]; desc != "" {
			cmd = append(cmd, "--description="+desc)
		}
		if expiry := params["date_expires"]; expiry != "" {
			cmd = append(cmd, "--date_expires="+expiry)
		}
		if minAmount := params["minimum_amount"]; minAmount != "" {
			cmd = append(cmd, "--minimum_amount="+minAmount)
		}
		if maxUses := params["usage_limit"]; maxUses != "" {
			cmd = append(cmd, "--usage_limit="+maxUses)
		}
		if perUser := params["usage_limit_per_user"]; perUser != "" {
			cmd = append(cmd, "--usage_limit_per_user="+perUser)
		}
		return cmd, nil

	case "delete_coupon":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "wc", "shop_coupon", "delete", id, "--force=true", "--user=1", "--allow-root"}, nil

	// ── WooCommerce: Customers ───────────────────────────────────
	case "list_customers":
		return []string{"wp", "wc", "customer", "list", "--user=1", "--format=json", "--allow-root"}, nil

	case "get_customer":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := validatePostID(id); err != nil {
			return nil, err
		}
		return []string{"wp", "wc", "customer", "get", id, "--user=1", "--format=json", "--allow-root"}, nil

	// ── Shipping zones ───────────────────────────────────────────
	case "list_shipping_zones":
		return []string{"wp", "wc", "shipping_zone", "list", "--user=1", "--format=json", "--allow-root"}, nil

	case "create_shipping_zone":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		cmd := []string{"wp", "wc", "shipping_zone", "create", "--name=" + name, "--user=1", "--porcelain", "--allow-root"}
		if order := params["order"]; order != "" {
			cmd = append(cmd, "--order="+order)
		}
		return cmd, nil

	case "list_shipping_zone_methods":
		zoneID := params["zone_id"]
		if zoneID == "" {
			return nil, fmt.Errorf("zone_id is required")
		}
		return []string{"wp", "wc", "shipping_zone_method", "list", zoneID, "--user=1", "--format=json", "--allow-root"}, nil

	case "add_shipping_method":
		zoneID := params["zone_id"]
		methodID := params["method_id"]
		if zoneID == "" || methodID == "" {
			return nil, fmt.Errorf("zone_id and method_id are required")
		}
		if methodID != "flat_rate" && methodID != "free_shipping" && methodID != "local_pickup" {
			return nil, fmt.Errorf("method_id must be 'flat_rate', 'free_shipping', or 'local_pickup'")
		}
		return []string{"wp", "wc", "shipping_zone_method", "create", zoneID, "--method_id=" + methodID, "--user=1", "--porcelain", "--allow-root"}, nil

	// ── Product variations ───────────────────────────────────────
	case "list_product_variations":
		productID := params["product_id"]
		if productID == "" {
			return nil, fmt.Errorf("product_id is required")
		}
		if err := validatePostID(productID); err != nil {
			return nil, err
		}
		return []string{"wp", "wc", "product_variation", "list", productID, "--user=1", "--format=json", "--allow-root"}, nil

	case "create_product_variation":
		productID := params["product_id"]
		if productID == "" {
			return nil, fmt.Errorf("product_id is required")
		}
		if err := validatePostID(productID); err != nil {
			return nil, err
		}
		cmd := []string{"wp", "wc", "product_variation", "create", productID, "--user=1", "--porcelain", "--allow-root"}
		if price := params["regular_price"]; price != "" {
			if err := validateWCPrice("regular_price", price); err != nil {
				return nil, err
			}
			cmd = append(cmd, "--regular_price="+price)
		}
		if sku := params["sku"]; sku != "" {
			cmd = append(cmd, "--sku="+sku)
		}
		if stock := params["stock_quantity"]; stock != "" {
			cmd = append(cmd, "--manage_stock=true", "--stock_quantity="+stock)
		}
		// Attributes passed as JSON string e.g. '[{"id":1,"option":"Small"}]'
		if attrs := params["attributes"]; attrs != "" {
			cmd = append(cmd, "--attributes="+attrs)
		}
		return cmd, nil

	case "create_product_attribute":
		name := params["name"]
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		cmd := []string{"wp", "wc", "product_attribute", "create", "--name=" + name, "--user=1", "--porcelain", "--allow-root"}
		if slug := params["slug"]; slug != "" {
			cmd = append(cmd, "--slug="+slug)
		}
		return cmd, nil

	// ── Store notice ─────────────────────────────────────────────
	case "set_store_notice":
		message := params["message"]
		enabled := params["enabled"]
		if message == "" && enabled == "" {
			return nil, fmt.Errorf("message or enabled is required")
		}
		phpCode := ""
		if enabled == "false" || enabled == "no" {
			phpCode = `update_option('woocommerce_demo_store', 'no'); echo 'Store notice disabled';`
		} else {
			if message != "" {
				encoded := base64Encode(message)
				phpCode = `update_option('woocommerce_demo_store', 'yes'); update_option('woocommerce_demo_store_notice', base64_decode("` + encoded + `")); echo 'Store notice updated';`
			} else {
				phpCode = `update_option('woocommerce_demo_store', 'yes'); echo 'Store notice enabled';`
			}
		}
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	// ── Menu item management ─────────────────────────────────────
	case "list_menu_items":
		menuID := params["menu_id"]
		if menuID == "" {
			return nil, fmt.Errorf("menu_id is required")
		}
		return []string{"wp", "menu", "item", "list", menuID, "--format=json", "--allow-root"}, nil

	case "delete_menu_item":
		itemID := params["item_id"]
		if itemID == "" {
			return nil, fmt.Errorf("item_id is required")
		}
		if err := validatePostID(itemID); err != nil {
			return nil, err
		}
		return []string{"wp", "menu", "item", "delete", itemID, "--allow-root"}, nil

	case "update_menu_item":
		itemID := params["item_id"]
		if itemID == "" {
			return nil, fmt.Errorf("item_id is required")
		}
		if err := validatePostID(itemID); err != nil {
			return nil, err
		}
		cmd := []string{"wp", "menu", "item", "update", itemID, "--allow-root"}
		if position := params["position"]; position != "" {
			cmd = append(cmd, "--position="+position)
		}
		if title := params["title"]; title != "" {
			cmd = append(cmd, "--title="+title)
		}
		return cmd, nil

	// ── Plugin uninstall ─────────────────────────────────────────
	case "uninstall_plugin":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !pluginSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid plugin slug format")
		}
		return []string{"wp", "plugin", "uninstall", slug, "--allow-root"}, nil

	// ── Thumbnail regeneration ───────────────────────────────────
	case "regenerate_thumbnails":
		return []string{"wp", "media", "regenerate", "--yes", "--allow-root"}, nil

	// ── Payment gateway management ──────────────────────────────
	case "list_payment_gateways":
		phpCode := `
			if (!function_exists('WC')) { echo 'WooCommerce not active'; exit(1); }
			$gateways = WC()->payment_gateways()->payment_gateways();
			$result = array();
			foreach ($gateways as $id => $gw) {
				$result[] = array(
					'id' => $gw->id,
					'title' => $gw->get_title(),
					'enabled' => $gw->enabled,
					'description' => $gw->get_description(),
				);
			}
			echo json_encode($result);
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "toggle_payment_gateway":
		gatewayID := params["gateway_id"]
		enabled := params["enabled"]
		if gatewayID == "" || enabled == "" {
			return nil, fmt.Errorf("gateway_id and enabled are required")
		}
		if enabled != "yes" && enabled != "no" {
			return nil, fmt.Errorf("enabled must be 'yes' or 'no'")
		}
		// Allowlist of safe gateway IDs
		allowedGateways := map[string]bool{
			"cod": true, "bacs": true, "cheque": true,
			"paypal": true, "stripe": true, "razorpay": true,
		}
		if !allowedGateways[gatewayID] {
			return nil, fmt.Errorf("gateway %q is not supported; allowed: cod, bacs, cheque, paypal, stripe, razorpay", gatewayID)
		}
		encodedID := base64Encode(gatewayID)
		phpCode := `
			if (!function_exists('WC')) { echo 'WooCommerce not active'; exit(1); }
			$id = base64_decode("` + encodedID + `");
			$gateways = WC()->payment_gateways()->payment_gateways();
			if (!isset($gateways[$id])) { echo "Gateway not found"; exit(1); }
			$gw = $gateways[$id];
			$gw->enabled = "` + enabled + `";
			update_option($gw->get_option_key(), apply_filters('woocommerce_settings_api_sanitized_fields_' . $gw->id, $gw->settings));
			echo ($gw->enabled === 'yes' ? 'Enabled' : 'Disabled') . " " . $gw->get_title();
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	case "update_gateway_setting":
		gatewayID := params["gateway_id"]
		key := params["key"]
		value := params["value"]
		if gatewayID == "" || key == "" || value == "" {
			return nil, fmt.Errorf("gateway_id, key, and value are required")
		}
		allowedGateways := map[string]bool{"cod": true, "bacs": true, "cheque": true, "paypal": true, "stripe": true, "razorpay": true}
		if !allowedGateways[gatewayID] {
			return nil, fmt.Errorf("gateway %q is not supported", gatewayID)
		}
		allowedKeys := map[string]bool{"title": true, "description": true, "instructions": true}
		if !allowedKeys[key] {
			return nil, fmt.Errorf("key must be 'title', 'description', or 'instructions'")
		}
		encodedID := base64Encode(gatewayID)
		encodedVal := base64Encode(value)
		phpCode := `
			if (!function_exists('WC')) { echo 'WooCommerce not active'; exit(1); }
			$id = base64_decode("` + encodedID + `");
			$val = base64_decode("` + encodedVal + `");
			$gateways = WC()->payment_gateways()->payment_gateways();
			if (!isset($gateways[$id])) { echo "Gateway not found"; exit(1); }
			$gw = $gateways[$id];
			$gw->settings["` + key + `"] = $val;
			update_option($gw->get_option_key(), apply_filters('woocommerce_settings_api_sanitized_fields_' . $gw->id, $gw->settings));
			echo "Updated " . $gw->get_title() . " ` + key + `";
		`
		return []string{"wp", "eval", phpCode, "--allow-root"}, nil

	// ── Image import from base64 ─────────────────────────────────
	case "import_image_base64":
		data := params["data"]
		filename := params["filename"]
		title := params["title"]
		if data == "" || filename == "" {
			return nil, fmt.Errorf("data and filename are required")
		}
		// Validate filename (alphanumeric + hyphens + one extension)
		filenameRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+\.(png|jpg|jpeg|webp)$`)
		if !filenameRe.MatchString(filename) {
			return nil, fmt.Errorf("filename must be alphanumeric with .png, .jpg, .jpeg, or .webp extension")
		}
		// Size check: base64 data should be < 10MB (produces ~7.5MB image)
		if len(data) > 10*1024*1024 {
			return nil, fmt.Errorf("image data too large (max 10MB base64)")
		}
		// Write base64 data to temp file and import via WordPress media_handle_sideload
		importCode := `
			$data = base64_decode("` + data + `");
			if ($data === false) { echo "Invalid base64 data"; exit(1); }
			$tmp = "/tmp/` + filename + `";
			file_put_contents($tmp, $data);
			$id = media_handle_sideload(array(
				'name' => "` + filename + `",
				'tmp_name' => $tmp,
			), 0);
			if (is_wp_error($id)) { echo "Import failed: " . $id->get_error_message(); exit(1); }
		`
		if title != "" {
			encodedTitle := base64Encode(title)
			importCode += `wp_update_post(array('ID' => $id, 'post_title' => base64_decode("` + encodedTitle + `")));
			`
		}
		importCode += `echo $id;`
		return []string{"wp", "eval", importCode, "--allow-root"}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
}

// handleWPExec executes a structured WP-CLI tool inside a site's PHP container.
//
// POST /api/sites/:site/wp-exec
func (a *API) handleWPExec(c *gin.Context) {
	site := c.Param("site")

	var req WPExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tool is required"})
		return
	}

	// Validate site exists and is active
	s, err := a.db.GetSite(site)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch site"})
		return
	}
	if s.Status != "ACTIVE" && s.Status != "DOMAIN_ACTIVE" {
		c.JSON(http.StatusConflict, gin.H{"error": "site must be ACTIVE"})
		return
	}

	// Handle create_theme specially — PHP script too large for command args.
	if req.Tool == "create_theme" {
		output, exitCode, err := a.handleCreateTheme(site, req.Params)
		if err != nil {
			log.Printf("wp-exec error for site %s tool %s: %v", site, req.Tool, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp := WPExecResponse{Site: site, Tool: req.Tool, Output: output, ExitCode: exitCode}
		if exitCode != 0 {
			c.JSON(http.StatusUnprocessableEntity, resp)
			return
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// Handle import_image_base64 specially — data is too large for command args.
	// We write the decoded image to a temp file via a small stdin-based exec,
	// then run wp media import on it.
	if req.Tool == "import_image_chunk" {
		output, exitCode, err := a.handleImageImportChunk(site, req.Params)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp := WPExecResponse{Site: site, Tool: req.Tool, Output: output, ExitCode: exitCode}
		if exitCode != 0 {
			c.JSON(http.StatusUnprocessableEntity, resp)
			return
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	if req.Tool == "import_image_base64" {
		output, exitCode, err := a.handleImageImport(site, req.Params)
		if err != nil {
			log.Printf("wp-exec error for site %s tool %s: %v", site, req.Tool, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp := WPExecResponse{Site: site, Tool: req.Tool, Output: output, ExitCode: exitCode}
		if exitCode != 0 {
			c.JSON(http.StatusUnprocessableEntity, resp)
			return
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// Harness-v2 special cases (theme_tools.go, store_tools.go): staged writes
	// with a php -l gate, deterministic diff edits, the verification
	// primitives, and the deterministic store baseline.
	if handler, ok := map[string]func(string, map[string]string) (string, int, error){
		"write_theme_file":  a.handleWriteThemeFileGated,
		"edit_theme_file":   a.handleEditThemeFile,
		"verify_page":       a.handleVerifyPage,
		"verify_store":      a.handleVerifyStore,
		"prepare_store":     a.handlePrepareStore,
		"set_home_sections": a.handleSetHomeSections,
	}[req.Tool]; ok {
		output, exitCode, err := handler(site, req.Params)
		if err != nil {
			log.Printf("wp-exec error for site %s tool %s: %v", site, req.Tool, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp := WPExecResponse{Site: site, Tool: req.Tool, Output: output, ExitCode: exitCode}
		if exitCode != 0 {
			c.JSON(http.StatusUnprocessableEntity, resp)
			return
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// Activation guard (design §3.4): never switch a live site onto a scaffolded
	// custom theme that is missing required templates.
	if req.Tool == "activate_theme" && themeSlugRe.MatchString(req.Params["slug"]) {
		if refusal, gerr := a.activationGuard(site, req.Params["slug"]); gerr == nil && refusal != "" {
			c.JSON(http.StatusUnprocessableEntity, WPExecResponse{
				Site: site, Tool: req.Tool, Output: refusal, ExitCode: 1,
			})
			return
		}
	}

	// Build the WP-CLI command from structured tool + params
	cmd, err := buildTool(req.Tool, req.Params)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Wrap in sh -c to merge stderr into stdout so error messages are visible.
	// Without this, Docker mux separates stdout/stderr and WP-CLI errors get lost.
	quoted := make([]string, len(cmd))
	for i, arg := range cmd {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	wrappedCmd := []string{"sh", "-c", strings.Join(quoted, " ") + " 2>&1"}

	// Execute inside the PHP container with a 30s timeout
	output, exitCode, err := a.execWPCLI(site, wrappedCmd)
	if err != nil {
		log.Printf("wp-exec error for site %s tool %s: %v", site, req.Tool, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	resp := WPExecResponse{
		Site:     site,
		Tool:     req.Tool,
		Output:   output,
		ExitCode: exitCode,
	}

	if exitCode != 0 {
		c.JSON(http.StatusUnprocessableEntity, resp)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// stripDockerMuxAll is like stripDockerMux but includes both stdout (type 1) and stderr (type 2).
// This is needed for wp-exec where WP-CLI error messages go to stderr.
func stripDockerMuxAll(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		header := make([]byte, 8)
		for {
			_, err := io.ReadFull(r, header)
			if err != nil {
				if err == io.EOF {
					pw.Close()
				} else {
					pw.CloseWithError(err)
				}
				return
			}
			size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
			if header[0] == 1 || header[0] == 2 { // stdout or stderr
				if _, err := io.CopyN(pw, r, size); err != nil {
					pw.CloseWithError(err)
					return
				}
			} else {
				if _, err := io.CopyN(io.Discard, r, size); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
		}
	}()
	return pr
}

// handleImageImport decodes base64 image data, writes it into the container via
// Docker exec stdin, then runs wp media import to add it to the media library.
// handleImageImportChunk implements a transport-resilient upload: base64
// arrives in <=256KB parts (stage=begin|part|finish). Large single POSTs to
// the control plane die when the Tailscale path degrades to a lossy relay
// (observed 2026-07-09: >~500KB bodies reset mid-transfer while small calls
// pass) — chunking makes image import immune to path quality.
func (a *API) handleImageImportChunk(site string, params map[string]string) (string, int, error) {
	uploadID := params["upload_id"]
	stage := params["stage"]
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{6,64}$`).MatchString(uploadID) {
		return "", -1, fmt.Errorf("upload_id must be 6-64 url-safe chars")
	}
	staging := "/tmp/hp_upload_" + uploadID + ".b64"
	switch stage {
	case "begin":
		if err := a.writeContainerFile(site, staging, []byte{}); err != nil {
			return "", -1, err
		}
		return "upload started", 0, nil
	case "part":
		part := params["data"]
		if part == "" || len(part) > 300*1024 {
			return "", -1, fmt.Errorf("part must be 1..300KB of base64")
		}
		if err := a.appendContainerFile(site, staging, []byte(part)); err != nil {
			return "", -1, err
		}
		return "part appended", 0, nil
	case "finish":
		// Decode IN the container — reading the staged base64 back through
		// execWPCLI truncates at maxOutputBytes (the first version of this
		// tool corrupted every image that way, 2026-07-09).
		filename := params["filename"]
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]+\.(png|jpg|jpeg|webp)$`).MatchString(filename) {
			return "", -1, fmt.Errorf("filename must be alphanumeric with .png/.jpg/.jpeg/.webp extension")
		}
		tmpImg := "/tmp/hp_img_" + uploadID + "_" + filename
		// Cleanup is BEST-EFFORT and never part of the success chain: /tmp has
		// the sticky bit and the staging file may belong to a different exec
		// user — an rm failure must not fail a successful decode (it did,
		// 2026-07-09).
		out, exit, err := a.execWPCLI(site, []string{"sh", "-c",
			"base64 -d " + staging + " > " + tmpImg + " 2>&1 && stat -c %s " + tmpImg})
		if err != nil {
			return "", -1, err
		}
		if exit != 0 {
			return "", -1, fmt.Errorf("staged decode failed: %s", strings.TrimSpace(out))
		}
		importCmd := []string{"sh", "-c", fmt.Sprintf("wp media import '%s' --porcelain --allow-root 2>&1", tmpImg)}
		output, exitCode, ierr := a.execWPCLI(site, importCmd)
		if ierr != nil {
			return "", -1, fmt.Errorf("wp media import: %w", ierr)
		}
		if exitCode != 0 {
			return output, exitCode, nil
		}
		a.execWPCLI(site, []string{"sh", "-c", "rm -f " + staging + " " + tmpImg + " 2>/dev/null; true"})
		attachID := strings.TrimSpace(output)
		if title := params["title"]; title != "" {
			a.execWPCLI(site, []string{"sh", "-c", fmt.Sprintf("wp post update %s --post_title='%s' --allow-root 2>&1", attachID, strings.ReplaceAll(title, "'", ""))})
		}
		return attachID, 0, nil
	}
	return "", -1, fmt.Errorf("stage must be begin, part or finish")
}

func (a *API) handleImageImport(site string, params map[string]string) (string, int, error) {
	data := params["data"]
	filename := params["filename"]
	title := params["title"]
	if data == "" || filename == "" {
		return "", -1, fmt.Errorf("data and filename are required")
	}

	filenameRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+\.(png|jpg|jpeg|webp)$`)
	if !filenameRe.MatchString(filename) {
		return "", -1, fmt.Errorf("filename must be alphanumeric with .png/.jpg/.jpeg/.webp extension")
	}
	if len(data) > 10*1024*1024 {
		return "", -1, fmt.Errorf("image data too large (max 10MB base64)")
	}

	// Decode base64
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", -1, fmt.Errorf("invalid base64 data: %w", err)
	}

	containerName := PHPContainerName(site)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Step 1: Write decoded image to /tmp/<filename> in the container via stdin
	tmpPath := "/tmp/" + filename
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

	// Write image data to stdin, then close
	_, err = io.Copy(attachResp.Conn, bytes.NewReader(decoded))
	attachResp.CloseWrite()
	attachResp.Close()
	if err != nil {
		return "", -1, fmt.Errorf("write image data: %w", err)
	}

	// Wait for the write exec to finish
	for i := 0; i < 10; i++ {
		inspect, err := a.docker.ContainerExecInspect(ctx, execResp.ID)
		if err != nil {
			return "", -1, fmt.Errorf("exec inspect (write): %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return "", inspect.ExitCode, fmt.Errorf("failed to write image file")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Step 2: Import via wp media import
	importCmd := []string{"sh", "-c", fmt.Sprintf("wp media import '%s' --porcelain --allow-root 2>&1", tmpPath)}
	output, exitCode, err := a.execWPCLI(site, importCmd)
	if err != nil {
		return "", -1, fmt.Errorf("wp media import: %w", err)
	}

	if exitCode != 0 {
		return output, exitCode, nil
	}

	// Update title if provided
	attachmentID := strings.TrimSpace(output)
	if title != "" && attachmentID != "" {
		encodedTitle := base64Encode(title)
		titleCmd := []string{"sh", "-c", fmt.Sprintf(
			`wp eval 'wp_update_post(array("ID" => %s, "post_title" => base64_decode("%s"))); echo "ok";' --allow-root 2>&1`,
			attachmentID, encodedTitle,
		)}
		a.execWPCLI(site, titleCmd) // best-effort
	}

	// Clean up temp file (best-effort)
	cleanCmd := []string{"sh", "-c", "rm -f " + tmpPath}
	a.execWPCLI(site, cleanCmd)

	return attachmentID, 0, nil
}

// execWPCLI runs a WP-CLI command inside the php_<site> container and captures output.
// scrubToolNoise drops known-harmless warning lines that leak into tool
// outputs and pollute the model's context (observed distracting a live build):
// the container has no sendmail, and wp-cli's phar cannot unlink the sticky
// /tmp staging file it already processed.
func scrubToolNoise(out string) string {
	if out == "" {
		return out
	}
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/usr/sbin/sendmail: not found") ||
			(strings.Contains(line, "PHP Warning:") && strings.Contains(line, "unlink(/tmp/")) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func (a *API) execWPCLI(site string, cmd []string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containerName := PHPContainerName(site)

	execResp, err := a.docker.ContainerExecCreate(ctx, containerName, types.ExecConfig{
		User:         "33:33", // www-data:www-data — keep WP-CLI-created files owned by the user PHP-FPM runs as
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   "/var/www/html",
	})
	if err != nil {
		return "", -1, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := a.docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return "", -1, fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()

	// Read output, stripping Docker mux headers (capped at 1MB).
	// Unlike backups, we include stderr so WP-CLI errors are visible.
	raw := stripDockerMuxAll(attachResp.Reader)
	limited := io.LimitReader(raw, int64(maxOutputBytes))
	var buf strings.Builder
	n, err := io.Copy(&buf, limited)
	if err != nil {
		return "", -1, fmt.Errorf("read output: %w", err)
	}
	output := scrubToolNoise(strings.TrimSpace(buf.String()))
	if n >= int64(maxOutputBytes) {
		output += "\n[output truncated at 1MB]"
	}

	// Check exit code
	inspect, err := a.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return output, -1, fmt.Errorf("exec inspect: %w", err)
	}

	return output, inspect.ExitCode, nil
}
