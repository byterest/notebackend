package ai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const systemPrompt = `You are an expert HTML/CSS/JS generator. The user will describe a component, widget, chart, tool, or interactive element.

Rules:
1. Output ONLY a single complete HTML document. No explanation, no markdown fences, no extra text.
2. The HTML must be self-contained (inline CSS and JS). No external dependencies except CDN links via <script> or <link>.
3. Use modern CSS (flexbox, grid, variables). Make it visually polished.
4. Use vanilla JavaScript unless the user asks for a framework.
5. Make it responsive and interactive where appropriate.
6. Use a clean, modern aesthetic: system fonts, subtle shadows, rounded corners, soft colors.
7. The <body> should have margin:0 and the component should fill the available space.
8. If the user asks for a chart or visualization, use Canvas API or inline SVG. Do NOT use external chart libraries unless the user specifically mentions one.`

// Handler proxies AI requests.
type Handler struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewHandler creates an AI handler.
func NewHandler(baseURL, apiKey, model string) *Handler {
	return &Handler{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{},
	}
}

type request struct {
	Prompt string `json:"prompt"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type streamResponse struct {
	Content string `json:"content"`
}

// Generate streams the AI response as SSE.
func (h *Handler) Generate(c *gin.Context) {
	var req request
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}

	if h.apiKey == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "AI API key not configured"})
		return
	}

	body := chatRequest{
		Model: h.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: req.Prompt},
		},
		Stream: true,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal request"})
		return
	}

	apiURL := h.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request"})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+h.apiKey)

	resp, err := h.client.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("AI request failed: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("AI returned %d: %s", resp.StatusCode, string(bodyBytes))})
		return
	}

	// Stream SSE to client
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		var chatResp chatResponse
		if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
			continue
		}
		if len(chatResp.Choices) == 0 {
			continue
		}

		token := chatResp.Choices[0].Delta.Content
		if token != "" {
			event, err := json.Marshal(streamResponse{Content: token})
			if err != nil {
				continue
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", event)
			flusher.Flush()
		}
	}
}
