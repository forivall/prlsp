package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strings"
)

// --- Data types ---

type ReviewComment struct {
	DatabaseID int    `json:"database_id"`
	Body       string `json:"body"`
	Author     string `json:"author"`
}

type ReviewThread struct {
	ThreadID   string          `json:"thread_id"`
	Path       string          `json:"path"`
	Line       int             `json:"line"`
	IsResolved bool            `json:"is_resolved"`
	Comments   []ReviewComment `json:"comments"`
}

// --- Interface ---

type GitHub interface {
	FindPR(owner, repo, branch string) (prNumber int, headSHA string, ok bool)
	FetchReviewThreads(owner, repo string, pr int) []ReviewThread
	ResolveThread(threadID string) bool
	ReplyToComment(owner, repo string, pr, commentID int, body string) bool
	CreateReviewComment(owner, repo string, pr int, commitID, path string, line int, body string) bool
	CreateReviewThread(owner, repo string, pr int, path string, line int, body string) bool
}

// --- gh CLI helper ---

func runGH(args []string) (string, error) {
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			log.Printf("gh %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		} else {
			log.Printf("gh command error: %v", err)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// --- GitHubAPI (real implementation) ---

type GitHubAPI struct{}

const threadsQuery = `
query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          line
          path
          comments(first: 50) {
            nodes {
              databaseId
              body
              author { login }
            }
          }
        }
      }
    }
  }
}
`

func (g *GitHubAPI) FindPR(owner, repo, branch string) (int, string, bool) {
	prenv := os.Getenv("GH_PR")
	if prenv != "" {
		log.Printf("loading pr from $GH_ENV: %s", prenv)
		out, err := runGH([]string{
			"pr", "view", prenv,
			"--repo", fmt.Sprintf("%s/%s", owner, repo),
			"--json", "number,headRefOid",
		})
		if err != nil || out == "" {
			return 0, "", false
		}
		var pr struct {
			Number     int    `json:"number"`
			HeadRefOid string `json:"headRefOid"`
		}
		if err := json.Unmarshal([]byte(out), &pr); err != nil {
			return 0, "", false
		}
		return pr.Number, pr.HeadRefOid, true
	}
	out, err := runGH([]string{
		"pr", "list",
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--head", branch,
		"--json", "number,headRefOid",
		"--limit", "1",
	})
	if err != nil || out == "" {
		return 0, "", false
	}
	var prs []struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil || len(prs) == 0 {
		return 0, "", false
	}
	return prs[0].Number, prs[0].HeadRefOid, true
}

func (g *GitHubAPI) FetchReviewThreads(owner, repo string, pr int) []ReviewThread {
	out, err := runGH([]string{
		"api", "graphql",
		"-f", fmt.Sprintf("query=%s", threadsQuery),
		"-f", fmt.Sprintf("owner=%s", owner),
		"-f", fmt.Sprintf("repo=%s", repo),
		"-F", fmt.Sprintf("pr=%d", pr),
	})
	if err != nil {
		return nil
	}

	var data struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Line       *int   `json:"line"`
							Path       string `json:"path"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int    `json:"databaseId"`
									Body       string `json:"body"`
									Author     *struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		log.Printf("failed to parse threads response: %v", err)
		return nil
	}

	var threads []ReviewThread
	for _, node := range data.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if node.Line == nil {
			continue
		}
		var comments []ReviewComment
		for _, c := range node.Comments.Nodes {
			author := "ghost"
			if c.Author != nil {
				author = c.Author.Login
			}
			comments = append(comments, ReviewComment{
				DatabaseID: c.DatabaseID,
				Body:       c.Body,
				Author:     author,
			})
		}
		threads = append(threads, ReviewThread{
			ThreadID:   node.ID,
			Path:       node.Path,
			Line:       *node.Line,
			IsResolved: node.IsResolved,
			Comments:   comments,
		})
	}
	return threads
}

func (g *GitHubAPI) ResolveThread(threadID string) bool {
	mutation := `
	mutation($threadId: ID!) {
	  resolveReviewThread(input: {threadId: $threadId}) {
	    thread { isResolved }
	  }
	}
	`
	_, err := runGH([]string{
		"api", "graphql",
		"-f", fmt.Sprintf("query=%s", mutation),
		"-f", fmt.Sprintf("threadId=%s", threadID),
	})
	return err == nil
}

func (g *GitHubAPI) ReplyToComment(owner, repo string, pr, commentID int, body string) bool {
	_, err := runGH([]string{
		"api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, pr, commentID),
		"-f", fmt.Sprintf("body=%s", body),
	})
	return err == nil
}

func (g *GitHubAPI) CreateReviewComment(owner, repo string, pr int, commitID, path string, line int, body string) bool {
	payload, _ := json.Marshal(map[string]interface{}{
		"commit_id": commitID,
		"body":      "",
		"event":     "COMMENT",
		"comments": []map[string]interface{}{
			{
				"path": path,
				"line": line,
				"side": "RIGHT",
				"body": body,
			},
		},
	})
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, pr),
		"--input", "-")
	cmd.Stdin = strings.NewReader(string(payload))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("create_review_comment failed: %s", strings.TrimSpace(string(out)))
		return false
	}
	return true
}

func (g *GitHubAPI) CreateReviewThread(owner, repo string, pr int, path string, line int, body string) bool {
	query := `query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) { id }
  }
}`
	result, err := runGH([]string{
		"api", "graphql",
		"-f", fmt.Sprintf("owner=%s", owner),
		"-f", fmt.Sprintf("repo=%s", repo),
		"-F", fmt.Sprintf("pr=%d", pr),
		"-f", fmt.Sprintf("query=%s", query),
	})
	if err != nil {
		return false
	}
	var response struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ID string `json:"id"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil || response.Data.Repository.PullRequest.ID == "" {
		log.Printf("failed to get pull request ID for review thread")
		return false
	}

	mutation := `mutation($body: String!, $pullRequestId: ID!, $path: String!, $line: Int!, $side: DiffSide = RIGHT, $subjectType: PullRequestReviewThreadSubjectType = LINE) {
  addPullRequestReviewThread(input: {
    body: $body,
    pullRequestId: $pullRequestId,
    path: $path,
    line: $line,
    side: $side,
    subjectType: $subjectType
  }) { thread { id } }
}`
	_, err = runGH([]string{
		"api", "graphql",
		"-f", fmt.Sprintf("body=%s", body),
		"-f", fmt.Sprintf("pullRequestId=%s", response.Data.Repository.PullRequest.ID),
		"-f", fmt.Sprintf("path=%s", path),
		"-F", fmt.Sprintf("line=%d", line),
		"-f", fmt.Sprintf("query=%s", mutation),
	})
	return err == nil
}

// --- MockGitHub (test double) ---

type MockGitHub struct {
	threads []ReviewThread
}

func NewMockGitHub(fixturePath string) (*MockGitHub, error) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	var fixture struct {
		Threads []ReviewThread `json:"threads"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return nil, fmt.Errorf("parse fixture: %w", err)
	}
	return &MockGitHub{threads: fixture.Threads}, nil
}

func (m *MockGitHub) FindPR(owner, repo, branch string) (int, string, bool) {
	return 1, "mock_sha", true
}

func (m *MockGitHub) FetchReviewThreads(owner, repo string, pr int) []ReviewThread {
	return m.threads
}

func (m *MockGitHub) ResolveThread(threadID string) bool {
	for i := range m.threads {
		if m.threads[i].ThreadID == threadID {
			m.threads[i].IsResolved = true
			return true
		}
	}
	return false
}

func (m *MockGitHub) ReplyToComment(owner, repo string, pr, commentID int, body string) bool {
	for i := range m.threads {
		for _, c := range m.threads[i].Comments {
			if c.DatabaseID == commentID {
				m.threads[i].Comments = append(m.threads[i].Comments, ReviewComment{
					DatabaseID: commentID + 1000,
					Body:       body,
					Author:     "you",
				})
				return true
			}
		}
	}
	return false
}

func (m *MockGitHub) CreateReviewComment(owner, repo string, pr int, commitID, path string, line int, body string) bool {
	m.threads = append(m.threads, ReviewThread{
		ThreadID:   fmt.Sprintf("PRRT_mock_%d", rand.Intn(9000)+1000),
		Path:       path,
		Line:       line,
		IsResolved: false,
		Comments:   []ReviewComment{{DatabaseID: rand.Intn(1000) + 9000, Body: body, Author: "you"}},
	})
	return true
}

func (m *MockGitHub) CreateReviewThread(owner, repo string, pr int, path string, line int, body string) bool {
	return m.CreateReviewComment(owner, repo, pr, "", path, line, body)
}
