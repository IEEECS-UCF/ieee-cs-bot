// Package purelymail is a thin client for the purelymail api calls the mail
// feature uses: users, password reset methods and routing rules.
package purelymail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

func New(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://purelymail.com/api/v0",
		token:      token,
	}
}

// an api error has http status 200: the envelope's type field decides.
func (c *Client) post(ctx context.Context, call string, request, result any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/"+call, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Purelymail-Api-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", call, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", call, err)
	}
	var envelope struct {
		Type    string          `json:"type"`
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Type == "" {
		return fmt.Errorf("%s: the response is not the api envelope (status %d)", call, resp.StatusCode)
	}
	if envelope.Type == "error" {
		return &APIError{Code: envelope.Code, Message: envelope.Message}
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("%s: bad result: %w", call, err)
		}
	}
	return nil
}
