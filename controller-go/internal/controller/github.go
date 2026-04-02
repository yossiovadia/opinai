package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // "open" or "closed"
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

// FetchOpenIssues returns open issues (not PRs) without the done label.
func FetchOpenIssues(repo, doneLabel string) ([]Issue, error) {
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/issues?state=open&per_page=100", repo))
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
		// Skip PRs
		if issue.PullRequest != nil {
			continue
		}
		// Skip issues with done label
		hasLabel := false
		for _, lbl := range issue.Labels {
			if lbl.Name == doneLabel {
				hasLabel = true
				break
			}
		}
		if hasLabel {
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

// RemoveLabel removes a label from an issue.
func RemoveLabel(repo string, number int, label string) {
	ghDelete(fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, label))
}
