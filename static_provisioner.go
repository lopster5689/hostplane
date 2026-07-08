package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

type StaticProvisioner struct {
	docker *client.Client
	cfg    Config
}

func NewStaticProvisioner(docker *client.Client, cfg Config) *StaticProvisioner {
	return &StaticProvisioner{docker: docker, cfg: cfg}
}

// Run provisions a static site.
// Files from the uploaded zip are extracted into the shared caddy_static_sites
// volume under /{site}/, then a Caddy snippet is written and Caddy is reloaded.
// No per-site container is created — Caddy's file_server handles serving directly.
func (p *StaticProvisioner) Run(site, zipPath string) error {
	domain := SiteDomain(site, p.cfg.BaseDomain)

	var filesUploaded, caddyWritten bool

	rollback := func(reason error) error {
		log.Printf("[rollback] triggered for static %s: %v", site, reason)

		if caddyWritten {
			p.removeCaddyConfig(site)
			reloadCaddy(p.cfg)
		}
		if filesUploaded {
			p.removeStaticSiteFiles(site)
		}

		return fmt.Errorf("static provisioning failed (rolled back): %w", reason)
	}

	// Step 1: extract zip into caddy_static_sites volume under /{site}/
	if err := p.extractZipToVolume(site, zipPath); err != nil {
		return rollback(fmt.Errorf("uploadZip: %w", err))
	}
	filesUploaded = true

	// Step 2: write Caddy snippet that serves /srv/sites/{site} via file_server
	if err := p.writeCaddyConfig(site, domain); err != nil {
		return rollback(fmt.Errorf("writeCaddyConfig: %w", err))
	}
	caddyWritten = true

	// Step 3: reload Caddy
	if err := reloadCaddy(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadCaddy: %w", err))
	}

	// Step 4: Poll for TLS cert readiness (non-fatal — Caddy retries in background).
	certStatus := PollCaddyCert(p.docker, p.cfg, domain, 30*time.Second)
	log.Printf("[static_provisioner] site=%s cert_status=%s", site, certStatus)

	os.Remove(zipPath)
	return nil
}

// extractZipToVolume extracts the zip into the shared caddy_static_sites
// Docker volume under the given destination subdirectory (/{destDir}/).
// It uses a temporary busybox container to perform the copy. destDir is both
// the on-volume subdirectory and the tar prefix, so passing a staging name
// (e.g. "{site}__new") extracts the files there instead of over the live site.
func (p *StaticProvisioner) extractZipToVolume(destDir, zipPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tmpName := "tmp_static_" + destDir

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{Image: "busybox", Cmd: []string{"sh"}},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: p.cfg.CaddyStaticVolume,
					Target: "/data",
				},
			},
		},
		nil, nil, tmpName,
	)
	if err != nil {
		return fmt.Errorf("create temp container: %w", err)
	}
	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	tarBuf, err := zipToTar(zipPath, destDir)
	if err != nil {
		return fmt.Errorf("zip to tar: %w", err)
	}

	// Copy to /data/ with files prefixed as {destDir}/<file> so Docker creates
	// the subdirectory automatically — /data/{destDir}/ becomes /srv/sites/{destDir}/ in Caddy
	if err = p.docker.CopyToContainer(ctx, resp.ID, "/data/", tarBuf, types.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	return nil
}

// RunUpdate replaces the files of an already-provisioned static site with the
// contents of a freshly uploaded zip, without touching the Caddy config or the
// TLS cert (the domain is unchanged). To keep the live site serving during the
// swap, the new files are extracted into a staging directory first and only
// moved into place once extraction succeeds — so a bad/corrupt zip leaves the
// old site untouched. Caddy's file_server reads from disk per request, so no
// reload is required.
func (p *StaticProvisioner) RunUpdate(site, zipPath string) error {
	staging := site + "__new"

	// Clear any leftover staging dir from a previously interrupted update.
	p.removeStaticSiteFiles(staging)

	// Step 1: extract the new zip into the staging directory.
	if err := p.extractZipToVolume(staging, zipPath); err != nil {
		p.removeStaticSiteFiles(staging)
		return fmt.Errorf("static update failed (live site untouched): %w", err)
	}

	// Step 2: atomically swap staging into place: rm old, mv new.
	if err := p.swapStaticDir(staging, site); err != nil {
		p.removeStaticSiteFiles(staging)
		return fmt.Errorf("static update swap failed: %w", err)
	}

	os.Remove(zipPath)
	log.Printf("[static_provisioner] site=%s files updated", site)
	return nil
}

// swapStaticDir replaces /data/{site} with /data/{staging} inside the
// caddy_static_sites volume using a single busybox container, so the
// remove-then-move is a tight, fast sequence with minimal serving gap.
func (p *StaticProvisioner) swapStaticDir(staging, site string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpName := "tmp_swapstatic_" + site
	swapCmd := fmt.Sprintf("rm -rf /data/%s && mv /data/%s /data/%s", site, staging, site)

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "busybox",
			Cmd:   []string{"sh", "-c", swapCmd},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: p.cfg.CaddyStaticVolume,
					Target: "/data",
				},
			},
		},
		nil, nil, tmpName,
	)
	if err != nil {
		return fmt.Errorf("create swap container: %w", err)
	}
	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	if err := p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start swap container: %w", err)
	}

	statusCh, errCh := p.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait swap container: %w", err)
		}
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("swap command exited %d", st.StatusCode)
		}
	}
	return nil
}

// removeStaticSiteFiles deletes /data/{site}/ from the shared caddy_static_sites
// volume using a temporary busybox container.
func (p *StaticProvisioner) removeStaticSiteFiles(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpName := "tmp_rmstatic_" + site

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "busybox",
			Cmd:   []string{"rm", "-rf", "/data/" + site},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: p.cfg.CaddyStaticVolume,
					Target: "/data",
				},
			},
		},
		nil, nil, tmpName,
	)
	if err != nil {
		log.Printf("[rollback] cannot create rm container for static %s: %v", site, err)
		return
	}
	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})

	statusCh, errCh := p.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("[rollback] wait error removing static files for %s: %v", site, err)
		}
	case <-statusCh:
	}
	log.Printf("[rollback] removed static site files for %s", site)
}

// zipToTar reads a zip and produces a tar stream with files prefixed by
// sitePrefix, suitable for streaming into the caddy_static_sites volume.
//
// Two layout fixes happen here so static sites "just work" regardless of
// how the user prepared the zip (see CAVEATS C-14):
//
//  1. Junk files are dropped: __MACOSX/* metadata, .DS_Store, Thumbs.db.
//  2. If every real file shares one top-level directory (the classic
//     "I zipped my project folder" case), that wrapper is stripped so
//     index.html lands at the site root where file_server expects it.
//
// If multiple top-level dirs exist or a file already sits at the zip root,
// no wrapper-stripping happens — the structure is preserved as-is.
func zipToTar(zipPath, sitePrefix string) (io.Reader, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	// Pass 1: figure out if there's a single wrapping top-level dir.
	wrapper := detectWrapperDir(zr.File)
	if wrapper != "" {
		log.Printf("[static] zip wrapper detected, stripping prefix %q for site %s", wrapper, sitePrefix)
	}

	// Pass 2: stream real files (skipping junk) with the wrapper stripped
	// and the site prefix prepended.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isZipJunk(f.Name) {
			continue
		}

		name := f.Name
		if wrapper != "" {
			name = strings.TrimPrefix(name, wrapper)
		}
		if name == "" {
			// the wrapper itself appeared as a zero-length entry; skip
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name: sitePrefix + "/" + name,
			Mode: 0644,
			Size: int64(len(content)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// detectWrapperDir returns the common top-level directory (with trailing
// slash) shared by all non-junk files in the zip, or "" if there is no
// single wrapper. We strip the wrapper so a project zipped as a folder
// behaves the same as one zipped from inside the folder.
func detectWrapperDir(files []*zip.File) string {
	var wrapper string
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if isZipJunk(f.Name) {
			continue
		}
		idx := strings.IndexByte(f.Name, '/')
		if idx == -1 {
			// File at the zip root → no wrapper to strip.
			return ""
		}
		seg := f.Name[:idx+1]
		if wrapper == "" {
			wrapper = seg
		} else if wrapper != seg {
			// More than one top-level dir → ambiguous, keep as-is.
			return ""
		}
	}
	return wrapper
}

// isZipJunk returns true for files we never want to extract — macOS Finder
// metadata and Windows thumbnail caches.
func isZipJunk(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	base := filepath.Base(name)
	switch base {
	case ".DS_Store", "Thumbs.db", "desktop.ini":
		return true
	}
	return false
}

// writeCaddyConfig writes a Caddy snippet that serves the static site via
// file_server. The Caddy container must have caddy_static_sites mounted at
// /srv/sites, so each site's files live at /srv/sites/{site}/.
func (p *StaticProvisioner) writeCaddyConfig(site, defaultDomain string, customDomain ...string) error {
	hosts := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		hosts = defaultDomain + ", " + customDomain[0] + ", www." + customDomain[0]
	}

	conf := fmt.Sprintf(`%s {
    root * /srv/sites/%s
    file_server
    encode gzip
}
`, hosts, site)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	tw.WriteHeader(&tar.Header{
		Name:    CaddyConfFile(site),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	})
	tw.Write(content)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureCaddyConfDir(ctx, p.docker, p.cfg.CaddyContainer, p.cfg.CaddyConfDir); err != nil {
		return err
	}

	return p.docker.CopyToContainer(ctx, p.cfg.CaddyContainer,
		p.cfg.CaddyConfDir, &buf, types.CopyToContainerOptions{})
}

// removeCaddyConfig removes the per-site Caddy snippet from the Caddy container.
func (p *StaticProvisioner) removeCaddyConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := p.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		log.Printf("[rollback] cannot create exec for caddy config removal (static %s): %v", site, err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[rollback] removed caddy config for static %s", site)
}
