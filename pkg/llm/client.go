package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Config holds the configuration for the LLM client
type Config struct {
	BaseURL       string
	APIKey        string
	Model         string
	Temperature   float64
	MaxTokens     int
	ContextLength int // n_ctx for LM Studio
	Timeout       time.Duration
}

// Client is the LLM client
type Client struct {
	config     Config
	httpClient *http.Client
}

// NewClient creates a new LLM client
func NewClient(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest represents the OpenAI chat completion request
type ChatRequest struct {
	Model         string    `json:"model"`
	Messages      []Message `json:"messages"`
	Temperature   float64   `json:"temperature"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	Stream        bool      `json:"stream"`
	ContextLength int       `json:"n_ctx,omitempty"` // LM Studio context length
}

// ChatResponse represents the OpenAI chat completion response
type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat sends a chat request to the LLM
func (c *Client) Chat(messages []Message) (string, error) {
	reqBody := ChatRequest{
		Model:         c.config.Model,
		Messages:      messages,
		Temperature:   c.config.Temperature,
		MaxTokens:     c.config.MaxTokens,
		ContextLength: c.config.ContextLength,
		Stream:        false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", c.config.BaseURL)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.config.APIKey))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("API returned error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return chatResp.Choices[0].Message.Content, nil
}
