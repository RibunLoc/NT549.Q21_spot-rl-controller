// Package jenkins — Jenkins API client cho controller.
// Dùng để drain agent trước khi terminate Spot instance (migrate).
package jenkins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client giao tiếp với Jenkins Master qua REST API.
type Client struct {
	baseURL    string
	user       string
	token      string
	httpClient *http.Client
}

// AgentStatus trạng thái của 1 Jenkins agent node.
type AgentStatus struct {
	Name         string `json:"displayName"`
	Offline      bool   `json:"offline"`
	Idle         bool   `json:"idle"`
	NumExecutors int    `json:"numExecutors"`
}

// QueueDepth số build job đang chờ executor.
type QueueDepth struct {
	Pending int // chờ được assign agent
	Running int // đang chạy trên agent
}

func NewClient(baseURL, user, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		token:   token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetQueueDepth đọc số jobs đang pending + running từ Jenkins.
func (c *Client) GetQueueDepth(ctx context.Context) (QueueDepth, error) {
	// Queue items: jobs chờ executor
	type queueItem struct {
		Blocked  bool `json:"blocked"`
		Buildable bool `json:"buildable"`
	}
	type queueResp struct {
		Items []queueItem `json:"items"`
	}

	body, err := c.get(ctx, "/queue/api/json?tree=items[blocked,buildable]")
	if err != nil {
		return QueueDepth{}, fmt.Errorf("queue api: %w", err)
	}

	var q queueResp
	if err := json.Unmarshal(body, &q); err != nil {
		return QueueDepth{}, fmt.Errorf("queue parse: %w", err)
	}

	// Running jobs: đếm executors đang busy trên tất cả nodes
	type executorResp struct {
		Computer []struct {
			Executors []struct {
				Idle bool `json:"idle"`
			} `json:"executors"`
		} `json:"computer"`
	}

	body2, err := c.get(ctx, "/computer/api/json?tree=computer[executors[idle]]")
	if err != nil {
		return QueueDepth{}, fmt.Errorf("computer api: %w", err)
	}

	var ex executorResp
	if err := json.Unmarshal(body2, &ex); err != nil {
		return QueueDepth{}, fmt.Errorf("executor parse: %w", err)
	}

	running := 0
	for _, node := range ex.Computer {
		for _, e := range node.Executors {
			if !e.Idle {
				running++
			}
		}
	}

	return QueueDepth{
		Pending: len(q.Items),
		Running: running,
	}, nil
}

// GetAgentStatus lấy trạng thái 1 agent theo tên.
func (c *Client) GetAgentStatus(ctx context.Context, agentName string) (AgentStatus, error) {
	path := fmt.Sprintf("/computer/%s/api/json?tree=displayName,offline,idle,numExecutors",
		url.PathEscape(agentName))

	body, err := c.get(ctx, path)
	if err != nil {
		return AgentStatus{}, fmt.Errorf("agent status: %w", err)
	}

	var s AgentStatus
	if err := json.Unmarshal(body, &s); err != nil {
		return AgentStatus{}, fmt.Errorf("agent status parse: %w", err)
	}
	return s, nil
}

// DrainAgent đánh dấu agent offline + đợi jobs hiện tại finish.
// Sau khi drain xong, instance có thể terminate an toàn.
//
// timeout: thời gian tối đa đợi drain (thường 5-10 phút)
func (c *Client) DrainAgent(ctx context.Context, agentName string, timeout time.Duration) error {
	// Bước 1: mark agent "temporarily offline" — Jenkins sẽ không assign job mới
	if err := c.markOffline(ctx, agentName, "RL Controller: migrate to cheaper pool"); err != nil {
		return fmt.Errorf("mark offline: %w", err)
	}

	// Bước 2: đợi agent idle (jobs đang chạy finish)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := c.GetAgentStatus(ctx, agentName)
		if err != nil {
			return fmt.Errorf("poll agent status: %w", err)
		}

		if status.Idle {
			return nil // drain xong, an toàn để terminate
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			// tiếp tục poll
		}
	}

	return fmt.Errorf("drain timeout after %s — agent %s still busy", timeout, agentName)
}

// DeleteAgent xóa agent node khỏi Jenkins sau khi instance đã terminate.
func (c *Client) DeleteAgent(ctx context.Context, agentName string) error {
	path := fmt.Sprintf("/computer/%s/doDelete", url.PathEscape(agentName))
	return c.post(ctx, path, "")
}

// markOffline gửi lệnh "temporarily offline" với message lý do.
func (c *Client) markOffline(ctx context.Context, agentName, reason string) error {
	path := fmt.Sprintf("/computer/%s/toggleOffline?offlineMessage=%s",
		url.PathEscape(agentName),
		url.QueryEscape(reason),
	)
	return c.post(ctx, path, "")
}

// ── HTTP helpers ──────────────────────────────────────

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("jenkins GET %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) post(ctx context.Context, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Jenkins CSRF protection — cần crumb
	crumb, err := c.getCrumb(ctx)
	if err == nil {
		req.Header.Set(crumb.Field, crumb.Value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("jenkins POST %s: status %d", path, resp.StatusCode)
	}
	return nil
}

type jenkinsCrumb struct {
	Field string `json:"crumbRequestField"`
	Value string `json:"crumb"`
}

func (c *Client) getCrumb(ctx context.Context) (jenkinsCrumb, error) {
	body, err := c.get(ctx, "/crumbIssuer/api/json")
	if err != nil {
		return jenkinsCrumb{}, err
	}
	var cr jenkinsCrumb
	return cr, json.Unmarshal(body, &cr)
}
