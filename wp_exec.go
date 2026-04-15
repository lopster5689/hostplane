package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/gin-gonic/gin"
)

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

// safeOptionKeys restricts which wp_options can be modified via the agent.
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
}

// pluginSlugRe validates wordpress.org plugin slugs (lowercase alphanumeric + hyphens).
var pluginSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// themeSlugRe validates wordpress.org theme slugs.
var themeSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// postFieldAllowlist restricts which post fields can be updated.
var postFieldAllowlist = map[string]bool{
	"post_title":   true,
	"post_content": true,
	"post_excerpt": true,
	"post_status":  true,
}

// buildTool maps a tool name + params to a safe WP-CLI argument array.
// Returns the []string to pass to ContainerExecCreate.Cmd.
func buildTool(tool string, params map[string]string) ([]string, error) {
	switch tool {
	case "get_site_info":
		return []string{"wp", "option", "get", "blogname", "--format=json", "--allow-root"}, nil

	case "get_option":
		key := params["key"]
		if key == "" {
			return nil, fmt.Errorf("key is required")
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
		return []string{"wp", "option", "update", key, value, "--allow-root"}, nil

	case "list_posts":
		postType := params["post_type"]
		if postType == "" {
			postType = "post"
		}
		if postType != "post" && postType != "page" {
			return nil, fmt.Errorf("post_type must be 'post' or 'page'")
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
		return []string{"wp", "post", "get", id, "--format=json", "--allow-root"}, nil

	case "update_post":
		id := params["id"]
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		field := params["field"]
		value := params["value"]
		if field == "" || value == "" {
			return nil, fmt.Errorf("field and value are required")
		}
		if !postFieldAllowlist[field] {
			return nil, fmt.Errorf("field %q is not allowed", field)
		}
		return []string{"wp", "post", "update", id, "--" + field + "=" + value, "--allow-root"}, nil

	case "create_post":
		postType := params["post_type"]
		title := params["post_title"]
		if postType == "" {
			postType = "page"
		}
		if postType != "post" && postType != "page" {
			return nil, fmt.Errorf("post_type must be 'post' or 'page'")
		}
		if title == "" {
			return nil, fmt.Errorf("post_title is required")
		}
		cmd := []string{"wp", "post", "create", "--post_type=" + postType, "--post_title=" + title, "--post_status=draft", "--allow-root"}
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

	case "activate_theme":
		slug := params["slug"]
		if slug == "" {
			return nil, fmt.Errorf("slug is required")
		}
		if !themeSlugRe.MatchString(slug) {
			return nil, fmt.Errorf("invalid theme slug format")
		}
		return []string{"wp", "theme", "activate", slug, "--allow-root"}, nil

	case "search_replace":
		old := params["old"]
		new := params["new"]
		if old == "" || new == "" {
			return nil, fmt.Errorf("old and new are required")
		}
		dryRun := params["dry_run"]
		cmd := []string{"wp", "search-replace", old, new, "--allow-root"}
		if dryRun == "true" {
			cmd = append(cmd, "--dry-run")
		}
		return cmd, nil

	case "media_import":
		url := params["url"]
		if url == "" {
			return nil, fmt.Errorf("url is required")
		}
		if !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("url must start with https://")
		}
		cmd := []string{"wp", "media", "import", url, "--allow-root"}
		if title := params["title"]; title != "" {
			cmd = append(cmd, "--title="+title)
		}
		return cmd, nil

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

	// Build the WP-CLI command from structured tool + params
	cmd, err := buildTool(req.Tool, req.Params)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Execute inside the PHP container with a 30s timeout
	output, exitCode, err := a.execWPCLI(site, cmd)
	if err != nil {
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

// execWPCLI runs a WP-CLI command inside the php_<site> container and captures output.
func (a *API) execWPCLI(site string, cmd []string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containerName := PHPContainerName(site)

	execResp, err := a.docker.ContainerExecCreate(ctx, containerName, types.ExecConfig{
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

	// Read output, stripping Docker mux headers
	raw := stripDockerMux(attachResp.Reader)
	var buf strings.Builder
	if _, err := io.Copy(&buf, raw); err != nil {
		return "", -1, fmt.Errorf("read output: %w", err)
	}
	output := strings.TrimSpace(buf.String())

	// Check exit code
	inspect, err := a.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return output, -1, fmt.Errorf("exec inspect: %w", err)
	}

	return output, inspect.ExitCode, nil
}
