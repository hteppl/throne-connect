package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"throne-connect/internal/logbuf"
)

// Client talks to a running node over its HTTP API. It has the same Status,
// Switch and Logs methods as Node.
type Client struct {
	base string
	http http.Client
}

func NewClient(apiListen string) *Client {
	return &Client{base: "http://" + apiListen}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("is the node running? %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("API: %s", resp.Status)
	}
	return json.Unmarshal(body, out)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/status", &st)
	return st, err
}

func (c *Client) Switch(ctx context.Context, target string) (string, error) {
	var m map[string]string
	if err := c.do(ctx, http.MethodPost, "/switch?to="+url.QueryEscape(target), &m); err != nil {
		return "", err
	}
	return m["active"], nil
}

func (c *Client) Logs(ctx context.Context, after uint64) ([]logbuf.Line, uint64, error) {
	var r LogsResponse
	if err := c.do(ctx, http.MethodGet, "/logs?after="+strconv.FormatUint(after, 10), &r); err != nil {
		return nil, after, err
	}
	return r.Lines, r.Next, nil
}
