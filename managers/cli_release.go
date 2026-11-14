package managers

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The CLI install on each platform downloads an asset from the CLI's latest
// GitHub release.
const cliInstallerRepo = "fosrl/cli"

type githubReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	// Digest is the "sha256:<hex>" GitHub computed when the asset was uploaded.
	Digest string `json:"digest"`
}

// maxCLIAssetSize bounds a CLI download.
const maxCLIAssetSize = 256 * 1024 * 1024

type githubReleaseResponse struct {
	TagName string               `json:"tag_name"`
	HTMLURL string               `json:"html_url"`
	Assets  []githubReleaseAsset `json:"assets"`
}

const cliInstallerUserAgent = "pangolin-client-cli-installer"

func getLatestCLIRelease(repo string) (*githubReleaseResponse, error) {
	repoParts := strings.Split(repo, "/")
	if len(repoParts) != 2 || repoParts[0] == "" || repoParts[1] == "" {
		return nil, fmt.Errorf("invalid repo %q, expected owner/name", repo)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoParts[0], repoParts[1])
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", cliInstallerUserAgent)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query GitHub releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub releases API returned %d: %s", resp.StatusCode, string(body))
	}

	var release githubReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to decode release response: %w", err)
	}
	return &release, nil
}

// assetURL returns the download URL of the release asset called name, and the
// sha256 the release API publishes for it. The CLI is installed as root or
// administrator, so an asset without a usable digest is refused.
func (r *githubReleaseResponse) assetURL(name string) (url, sha256Hex string, err error) {
	for _, asset := range r.Assets {
		if asset.Name == name && asset.URL != "" {
			sha256Hex, err = parseAssetDigest(asset.Digest)
			if err != nil {
				return "", "", fmt.Errorf("can't verify %s in release %s: %w", name, r.HTMLURL, err)
			}
			return asset.URL, sha256Hex, nil
		}
	}
	return "", "", fmt.Errorf("latest release does not include %s (release: %s)", name, r.HTMLURL)
}

// parseAssetDigest returns the hex sha256 of a GitHub "sha256:<hex>" digest.
func parseAssetDigest(digest string) (string, error) {
	hexSum, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("the release publishes no sha256 digest for it (got %q)", digest)
	}
	if b, err := hex.DecodeString(hexSum); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("the release publishes a malformed sha256 digest (%q)", digest)
	}
	return strings.ToLower(hexSum), nil
}

// downloadCLIAsset saves url to destPath, which it creates, and checks it
// against wantSHA256 (hex) before returning. A file that fails the check is removed.
func downloadCLIAsset(url, destPath, wantSHA256 string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create download request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", cliInstallerUserAgent)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download the CLI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CLI download failed with %d: %s", resp.StatusCode, string(body))
	}

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create download file: %w", err)
	}
	defer out.Close()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(resp.Body, maxCLIAssetSize+1))
	if err == nil && n > maxCLIAssetSize {
		err = fmt.Errorf("the download is larger than %d bytes", int64(maxCLIAssetSize))
	}
	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("failed to write download file: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(wantSHA256))) != 1 {
		os.Remove(destPath)
		return fmt.Errorf("the downloaded CLI has the wrong sha256 (got %s, the release says %s)", got, wantSHA256)
	}
	return out.Close()
}
