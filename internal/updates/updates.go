// Package updates checks the project's GitHub Releases for a newer version and
// returns the release notes. It does not download or install anything — the UI
// links the user to the installer asset.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub "owner/repo" the releases are published under.
const Repo = "YeBai466/B_Download_Manager"

// Result describes the outcome of an update check.
type Result struct {
	Current      string `json:"current"`      // running version
	Latest       string `json:"latest"`       // latest release tag (normalized, no leading v)
	HasUpdate    bool   `json:"hasUpdate"`    // Latest > Current
	Notes        string `json:"notes"`        // release body (markdown)
	ReleaseURL   string `json:"releaseUrl"`   // html_url of the release page
	DownloadURL  string `json:"downloadUrl"`  // installer asset URL (.exe), if found
	PublishedAt  string `json:"publishedAt"`
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check queries the latest GitHub release and compares it to current.
func Check(ctx context.Context, current string) (Result, error) {
	res := Result{Current: normalize(current)}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "BDownloadManager-UpdateCheck")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return res, fmt.Errorf("尚无已发布的版本")
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return res, err
	}

	res.Latest = normalize(rel.TagName)
	res.Notes = strings.TrimSpace(rel.Body)
	res.ReleaseURL = rel.HTMLURL
	res.PublishedAt = rel.PublishedAt
	for _, a := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
			res.DownloadURL = a.BrowserDownloadURL
			break
		}
	}
	res.HasUpdate = compare(res.Latest, res.Current) > 0
	return res, nil
}

// normalize strips a leading v/V and surrounding spaces from a version tag.
func normalize(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// compare returns 1 if a>b, -1 if a<b, 0 if equal, by dotted numeric segments.
func compare(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, bi := seg(as, i), seg(bs, i)
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return 0
}

func seg(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	// strip any pre-release suffix like "1-rc2"
	s := parts[i]
	if j := strings.IndexAny(s, "-+"); j >= 0 {
		s = s[:j]
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
