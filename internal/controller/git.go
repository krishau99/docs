package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitPoller handles fetching the latest commit SHA from a git repository.
type GitPoller struct {
	// httpClient is used for HTTP requests (with optional custom CA).
	httpClient *http.Client
}

// NewGitPoller creates a new GitPoller with optional custom CA certificate PEM data.
func NewGitPoller(caPEM []byte) *GitPoller {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	if len(caPEM) > 0 {
		rootCAs, err := x509.SystemCertPool()
		if err != nil {
			rootCAs = x509.NewCertPool()
		}
		rootCAs.AppendCertsFromPEM(caPEM)
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: rootCAs,
			},
		}
		client.Transport = transport
	}

	return &GitPoller{httpClient: client}
}

// GetLatestSHA returns the latest commit SHA for the given repository URL and branch.
// It first tries the smart HTTP git protocol (info/refs), then falls back to the
// Gitea REST API.
func (g *GitPoller) GetLatestSHA(ctx context.Context, repoURL, branch string, creds *GitCredentials) (string, error) {
	// Try git smart HTTP protocol first (works with any git hosting)
	sha, err := g.getLatestSHAViaGitHTTP(ctx, repoURL, branch, creds)
	if err == nil && sha != "" {
		return sha, nil
	}

	// Fall back to Gitea API if the URL looks like a Gitea instance
	if isGiteaURL(repoURL) {
		sha, apiErr := g.getLatestSHAViaGiteaAPI(ctx, repoURL, branch, creds)
		if apiErr == nil && sha != "" {
			return sha, nil
		}
		// Return the original error for better diagnostics
		return "", fmt.Errorf("git HTTP error: %w; Gitea API error: %v", err, apiErr)
	}

	return "", fmt.Errorf("failed to get latest SHA via git HTTP protocol: %w", err)
}

// GitCredentials holds authentication information for a git repository.
type GitCredentials struct {
	Username string
	Password string
}

// getLatestSHAViaGitHTTP uses the git smart HTTP protocol to get the latest commit SHA.
// It calls the /info/refs?service=git-upload-pack endpoint.
func (g *GitPoller) getLatestSHAViaGitHTTP(ctx context.Context, repoURL, branch string, creds *GitCredentials) (string, error) {
	// Normalize URL
	repoURL = strings.TrimSuffix(repoURL, "/")
	if !strings.HasSuffix(repoURL, ".git") {
		repoURL += ".git"
	}

	infoURL := repoURL + "/info/refs?service=git-upload-pack"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	return parseSHAFromGitInfoRefs(string(body), branch)
}

// parseSHAFromGitInfoRefs parses the SHA for a given branch from the git info/refs response.
// The format is a series of pkt-line encoded lines like:
// <4-byte-length><sha> <refname>\n
func parseSHAFromGitInfoRefs(body, branch string) (string, error) {
	// The response starts with a pkt-line header "# service=git-upload-pack\n"
	// followed by a "0000" flush packet, then the actual refs.
	// Each ref is: <4-hex-length><40-hex-sha> <refname>\n
	// We scan line by line looking for refs/heads/<branch>

	// Strip pkt-line lengths (4 hex chars at the start of each line)
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		// Skip the pkt-line length prefix (4 chars)
		content := line[4:]

		// Skip control lines
		if strings.HasPrefix(content, "#") || content == "" {
			continue
		}

		// Each ref line is: <sha> <refname>[optional NUL-separated capabilities]
		// SHA is always 40 hex chars followed by a space
		if len(content) < 41 {
			continue
		}

		sha := content[:40]
		rest := content[41:]

		// Strip NUL-delimited capabilities (only on the first ref line)
		if idx := strings.IndexByte(rest, 0); idx >= 0 {
			rest = rest[:idx]
		}
		rest = strings.TrimSpace(rest)

		// Match exact branch ref
		if rest == "refs/heads/"+branch {
			return sha, nil
		}
	}

	return "", fmt.Errorf("branch %q not found in git info/refs response", branch)
}

// giteaBranchResponse is the response from the Gitea branches API.
type giteaBranchResponse struct {
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// getLatestSHAViaGiteaAPI uses the Gitea REST API to get the latest commit SHA.
// URL format: https://<host>/api/v1/repos/<owner>/<repo>/branches/<branch>
func (g *GitPoller) getLatestSHAViaGiteaAPI(ctx context.Context, repoURL, branch string, creds *GitCredentials) (string, error) {
	apiURL, err := buildGiteaAPIURL(repoURL, branch)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Gitea API returned status %d", resp.StatusCode)
	}

	var result giteaBranchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding Gitea API response: %w", err)
	}

	if result.Commit.ID == "" {
		return "", fmt.Errorf("empty commit ID in Gitea API response")
	}

	return result.Commit.ID, nil
}

// buildGiteaAPIURL constructs the Gitea API URL from a repository clone URL.
// Example: https://gitea.internal/org/docs-a -> https://gitea.internal/api/v1/repos/org/docs-a/branches/main
func buildGiteaAPIURL(repoURL, branch string) (string, error) {
	// Normalize
	repoURL = strings.TrimSuffix(repoURL, "/")
	repoURL = strings.TrimSuffix(repoURL, ".git")

	// Extract scheme and host+path
	var scheme, rest string
	if strings.HasPrefix(repoURL, "https://") {
		scheme = "https://"
		rest = repoURL[8:]
	} else if strings.HasPrefix(repoURL, "http://") {
		scheme = "http://"
		rest = repoURL[7:]
	} else {
		return "", fmt.Errorf("unsupported URL scheme in %q", repoURL)
	}

	// Split host from path
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return "", fmt.Errorf("cannot parse host from URL %q", repoURL)
	}

	host := rest[:slashIdx]
	path := rest[slashIdx+1:] // e.g. "org/docs-a"

	return fmt.Sprintf("%s%s/api/v1/repos/%s/branches/%s", scheme, host, path, branch), nil
}

// isGiteaURL heuristically checks whether a URL might be a Gitea instance.
// We try the Gitea API for any https/http git URL since it's a graceful fallback.
func isGiteaURL(repoURL string) bool {
	return strings.HasPrefix(repoURL, "http://") || strings.HasPrefix(repoURL, "https://")
}
