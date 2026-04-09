package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const ghAPI = "https://api.github.com"

var httpClient = &http.Client{Timeout: 30 * time.Second}

func ghHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("Authorization", "Bearer "+os.Getenv("GITHUB_TOKEN"))
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	return h
}

func ghGet(path string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", ghAPI+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header = ghHeaders()
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func ghPost(path string, payload any) ([]byte, int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest("POST", ghAPI+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	req.Header = ghHeaders()
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func ghDelete(path string) (int, error) {
	req, err := http.NewRequest("DELETE", ghAPI+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header = ghHeaders()
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Issue represents a GitHub issue.
type Issue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

// FetchOpenIssues returns open issues (not PRs).
// If since is non-empty, only returns issues created/updated after that ISO timestamp.
// Filtering for "already processed" is done by the caller via database.IsProcessed.
func FetchOpenIssues(repo, since string) ([]Issue, error) {
	url := fmt.Sprintf("/repos/%s/issues?state=open&per_page=100&sort=created&direction=desc", repo)
	if since != "" {
		url += "&since=" + since
	}
	body, code, err := ghGet(url)
	if err != nil {
		return nil, fmt.Errorf("fetch issues for %s: %w", repo, err)
	}
	if code != 200 {
		return nil, fmt.Errorf("fetch issues for %s: HTTP %d", repo, code)
	}

	var all []Issue
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("parse issues for %s: %w", repo, err)
	}

	var filtered []Issue
	for _, issue := range all {
		if issue.PullRequest != nil {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered, nil
}

// FetchIssueDetails gets full details for a single issue.
func FetchIssueDetails(repo string, number int) (*Issue, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/issues/%d", repo, number))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// IssueComment represents a GitHub issue comment.
type IssueComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// FetchIssueComments returns the first 10 comments on an issue.
func FetchIssueComments(repo string, number int) ([]IssueComment, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=10", repo, number))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var comments []IssueComment
	for _, c := range raw {
		comments = append(comments, IssueComment{
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return comments, nil
}

// AddLabel adds a label to an issue.
func AddLabel(repo string, number int, label string) error {
	_, code, err := ghPost(
		fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number),
		map[string][]string{"labels": {label}},
	)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("HTTP %d adding label", code)
	}
	return nil
}

// PostComment posts a comment on an issue.
func PostComment(repo string, number int, body string) error {
	_, code, err := ghPost(
		fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		map[string]string{"body": body},
	)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("HTTP %d posting comment", code)
	}
	return nil
}

// GetRepoHeadSHA returns the latest commit SHA for a repo's default branch.
func GetRepoHeadSHA(repo string) (string, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/commits?per_page=1", repo))
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("HTTP %d", code)
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commits); err != nil || len(commits) == 0 {
		return "", fmt.Errorf("no commits found")
	}
	return commits[0].SHA, nil
}

var ghLinkRe = regexp.MustCompile(`https://github\.com/([^/]+/[^/]+)/(pull|issues)/(\d+)`)

// FetchLinkedResources scans text for GitHub PR/issue URLs and fetches their content
// using the gh CLI. Returns map of URL → content. Max 5 links, 8KB total.
func FetchLinkedResources(text string) map[string]string {
	matches := ghLinkRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	type link struct{ repo, kind, number string }
	var links []link
	for _, m := range matches {
		url := m[0]
		if seen[url] {
			continue
		}
		seen[url] = true
		links = append(links, link{repo: m[1], kind: m[2], number: m[3]})
		if len(links) >= 5 {
			break
		}
	}

	result := make(map[string]string)
	totalSize := 0
	const maxTotal = 8192

	for _, l := range links {
		if totalSize >= maxTotal {
			break
		}
		url := fmt.Sprintf("https://github.com/%s/%s/%s", l.repo, l.kind, l.number)
		var content string

		if l.kind == "pull" {
			out, err := exec.Command("gh", "pr", "view", l.number, "-R", l.repo,
				"--json", "title,body,files,additions,deletions").CombinedOutput()
			if err == nil {
				content = string(out)
			}
			diffOut, err := exec.Command("gh", "pr", "diff", l.number, "-R", l.repo).CombinedOutput()
			if err == nil && len(diffOut) > 0 {
				diff := string(diffOut)
				if len(diff) > 4096 {
					diff = diff[:4096] + "\n... (diff truncated)"
				}
				content += "\n\n--- PR Diff ---\n" + diff
			}
		} else {
			out, err := exec.Command("gh", "issue", "view", l.number, "-R", l.repo,
				"--json", "title,body").CombinedOutput()
			if err == nil {
				content = string(out)
			}
		}

		if content == "" {
			continue
		}
		if totalSize+len(content) > maxTotal {
			content = content[:maxTotal-totalSize] + "\n... (truncated)"
		}
		result[url] = content
		totalSize += len(content)
		slog.Info("fetched linked resource", "url", url, "bytes", len(content))
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// RemoveLabel removes a label from an issue.
func RemoveLabel(repo string, number int, label string) {
	ghDelete(fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, label))
}

// PullRequest represents a GitHub pull request.
type PullRequest struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// PRChangedFile represents a file changed in a PR.
type PRChangedFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // added, removed, modified, renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// FetchPRDetails returns full details for a pull request.
func FetchPRDetails(repo string, prNumber int) (*PullRequest, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/pulls/%d", repo, prNumber))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var pr PullRequest
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// FetchPRDiff returns the unified diff for a pull request.
func FetchPRDiff(repo string, prNumber int) (string, error) {
	req, err := http.NewRequest("GET", ghAPI+fmt.Sprintf("/repos/%s/pulls/%d", repo, prNumber), nil)
	if err != nil {
		return "", err
	}
	req.Header = ghHeaders()
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

// FetchPRChangedFiles returns the list of changed files in a PR.
func FetchPRChangedFiles(repo string, prNumber int) ([]PRChangedFile, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, prNumber))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var files []PRChangedFile
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// FetchPRComments returns issue-level comments on a pull request.
func FetchPRComments(repo string, prNumber int) ([]IssueComment, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=30", repo, prNumber))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var comments []IssueComment
	for _, c := range raw {
		comments = append(comments, IssueComment{
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return comments, nil
}

// PRReview represents a GitHub pull request review.
type PRReview struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	State  string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
}

// FetchPRReviews returns review objects for a pull request.
func FetchPRReviews(repo string, prNumber int) ([]PRReview, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=30", repo, prNumber))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var reviews []PRReview
	for _, r := range raw {
		if r.Body == "" {
			continue // Skip reviews with no body (e.g. bare approvals)
		}
		reviews = append(reviews, PRReview{
			Author: r.User.Login,
			Body:   r.Body,
			State:  r.State,
		})
	}
	return reviews, nil
}

// FetchPRInlineComments returns inline diff review comments on a pull request.
func FetchPRInlineComments(repo string, prNumber int) ([]IssueComment, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=30", repo, prNumber))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		Path      string `json:"path"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var comments []IssueComment
	for _, c := range raw {
		commentBody := c.Body
		if c.Path != "" {
			commentBody = fmt.Sprintf("[%s] %s", c.Path, commentBody)
		}
		comments = append(comments, IssueComment{
			Author:    c.User.Login,
			Body:      commentBody,
			CreatedAt: c.CreatedAt,
		})
	}
	return comments, nil
}

// PRCommentContext represents a comment or review for passing to the runner.
type PRCommentContext struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Type   string `json:"type"` // "comment", "review", "inline"
}

// CollectPRComments fetches all comments, reviews, and inline comments for a PR
// and returns them as a combined slice, truncating individual bodies to maxBodyLen.
func CollectPRComments(repo string, prNumber int, maxBodyLen int) []PRCommentContext {
	var result []PRCommentContext

	// Issue-level comments
	if comments, err := FetchPRComments(repo, prNumber); err == nil {
		for _, c := range comments {
			body := c.Body
			if len(body) > maxBodyLen {
				body = body[:maxBodyLen] + "..."
			}
			result = append(result, PRCommentContext{Author: c.Author, Body: body, Type: "comment"})
		}
	}

	// Review objects (body + state)
	if reviews, err := FetchPRReviews(repo, prNumber); err == nil {
		for _, r := range reviews {
			body := r.Body
			if r.State != "" {
				body = fmt.Sprintf("[%s] %s", r.State, body)
			}
			if len(body) > maxBodyLen {
				body = body[:maxBodyLen] + "..."
			}
			result = append(result, PRCommentContext{Author: r.Author, Body: body, Type: "review"})
		}
	}

	// Inline diff comments
	if inlines, err := FetchPRInlineComments(repo, prNumber); err == nil {
		for _, c := range inlines {
			body := c.Body
			if len(body) > maxBodyLen {
				body = body[:maxBodyLen] + "..."
			}
			result = append(result, PRCommentContext{Author: c.Author, Body: body, Type: "inline"})
		}
	}

	return result
}

// FormatPRCommentsForAgent formats collected PR comments into a human-readable
// string suitable for inclusion in the agent prompt.
func FormatPRCommentsForAgent(comments []PRCommentContext) string {
	if len(comments) == 0 {
		return ""
	}

	// Group by author
	type entry struct {
		bodies []string
	}
	authorOrder := []string{}
	grouped := map[string]*entry{}
	for _, c := range comments {
		if _, ok := grouped[c.Author]; !ok {
			authorOrder = append(authorOrder, c.Author)
			grouped[c.Author] = &entry{}
		}
		grouped[c.Author].bodies = append(grouped[c.Author].bodies, c.Body)
	}

	var sb strings.Builder
	for _, author := range authorOrder {
		e := grouped[author]
		sb.WriteString(fmt.Sprintf("### @%s:\n", author))
		for _, body := range e.bodies {
			sb.WriteString(body)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}
