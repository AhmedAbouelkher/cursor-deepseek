package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"cursor-deepseek/tunnel"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/http2"
)

const (
	deepseekEndpoint     = "https://api.deepseek.com"
	deepseekBetaEndpoint = "https://api.deepseek.com/beta"
	deepseekChatModel    = "deepseek-chat"
	deepseekCoderModel   = "deepseek-coder"
	gpt4oModel           = "gpt-4o"
)

var modelFlag string
var applicationPort int
var openTunnel bool
var verbose bool

// Configuration structure
type Config struct {
	endpoint string
	model    string
}

var activeConfig Config

type ReasoningEffort string

const (
	ReasoningEffortDefault ReasoningEffort = ""
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortMax     ReasoningEffort = "max"
)

var reasoningEffortMap = map[ReasoningEffort]string{
	ReasoningEffortLow:     "low",
	ReasoningEffortHigh:    "high",
	ReasoningEffortMax:     "max",
	ReasoningEffortDefault: "",
}

// Models response structure
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// OpenAI compatible request structure
type ChatRequest struct {
	Model       string      `json:"model"`
	Messages    []Message   `json:"messages"`
	Stream      bool        `json:"stream"`
	Functions   []Function  `json:"functions,omitempty"`
	Tools       []Tool      `json:"tools,omitempty"`
	ToolChoice  interface{} `json:"tool_choice,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
	MaxTokens   *int        `json:"max_tokens,omitempty"`
}

func (chatReq ChatRequest) ExtractReasoningEffort() (string, ReasoningEffort, bool) {
	model := chatReq.Model
	modelSegments := strings.Split(model, "$")
	if len(modelSegments) < 2 {
		return model, ReasoningEffortDefault, false
	}
	modelName := modelSegments[0]
	reasoningEffortStr := modelSegments[1]
	reasoningEffort, ok := reasoningEffortMap[ReasoningEffort(reasoningEffortStr)]
	if !ok {
		return model, ReasoningEffortDefault, false
	}
	hasThinking := len(modelSegments) > 2
	return modelName, ReasoningEffort(reasoningEffort), hasThinking
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// ContentPart represents a part of multimodal content
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// GetContentString extracts text content from Message.Content
// It handles both string and array formats
func (m *Message) GetContentString() string {
	if m.Content == nil {
		return ""
	}

	// Try to unmarshal as string first
	var strContent string
	if err := json.Unmarshal(m.Content, &strContent); err == nil {
		return strContent
	}

	// Try to unmarshal as array of content parts
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var textParts []string
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}
		return strings.Join(textParts, "\n")
	}

	return ""
}

// SetContentString sets the content as a string
func (m *Message) SetContentString(content string) {
	data, _ := json.Marshal(content)
	m.Content = data
}

type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func convertToolChoice(choice interface{}) string {
	if choice == nil {
		return ""
	}

	// If string "auto" or "none"
	if str, ok := choice.(string); ok {
		switch str {
		case "auto", "none":
			return str
		}
	}

	// Try to parse as map for function call
	if choiceMap, ok := choice.(map[string]interface{}); ok {
		if choiceMap["type"] == "function" {
			return "auto" // DeepSeek doesn't support specific function selection, default to auto
		}
	}

	return ""
}

func convertMessages(messages []Message) []Message {
	converted := make([]Message, len(messages))
	for i, msg := range messages {
		log.Printf("Converting message %d - Role: %s", i, msg.Role)
		converted[i] = msg

		// Convert array-format content to string format for DeepSeek
		// DeepSeek only supports string content, not multimodal arrays
		contentStr := msg.GetContentString()
		converted[i].SetContentString(contentStr)

		// Handle assistant messages with tool calls
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			log.Printf("Processing assistant message with %d tool calls", len(msg.ToolCalls))
			// DeepSeek expects tool_calls in a specific format
			toolCalls := make([]ToolCall, len(msg.ToolCalls))
			for j, tc := range msg.ToolCalls {
				toolCalls[j] = ToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: tc.Function,
				}
				log.Printf("Tool call %d - ID: %s, Function: %s", j, tc.ID, tc.Function.Name)
			}
			converted[i].ToolCalls = toolCalls
		}

		// Handle function response messages
		if msg.Role == "function" {
			log.Printf("Converting function response to tool response")
			// Convert to tool response format
			converted[i].Role = "tool"
		}
	}

	// Log the final converted messages
	for i, msg := range converted {
		log.Printf("Final message %d - Role: %s, Content: %s", i, msg.Role, truncateString(msg.GetContentString(), 50))
		if len(msg.ToolCalls) > 0 {
			log.Printf("Message %d has %d tool calls", i, len(msg.ToolCalls))
		}
	}

	return converted
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// DeepSeek request structure

type Thinking struct {
	Type string `json:"type"` // enabled/disabled
}
type DeepSeekRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	Temperature     float64         `json:"temperature,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
	ReasoningEffort ReasoningEffort `json:"reasoning_effort,omitempty"`
	Thinking        Thinking        `json:"thinking,omitempty"`
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

	flag.BoolVar(&openTunnel, "tunnel", false, "Open a Cloudflare tunnel")
	flag.StringVar(&modelFlag, "model", "chat", "The model to use")
	flag.IntVar(&applicationPort, "port", 9000, "The port to use")
	flag.BoolVar(&verbose, "verbose", false, "Verbose logging")
	flag.Parse()

	// Configure the active endpoint and model based on the flag
	switch modelFlag {
	case "coder":
		activeConfig = Config{
			endpoint: deepseekBetaEndpoint,
			model:    deepseekCoderModel,
		}
	case "chat":
		activeConfig = Config{
			endpoint: deepseekEndpoint,
			model:    deepseekChatModel,
		}
	default:
		log.Printf("Invalid model specified: %s. Using default chat model.", modelFlag)
		activeConfig = Config{
			endpoint: deepseekEndpoint,
			model:    deepseekChatModel,
		}
	}

	log.Printf("Initialized with model: %s using endpoint: %s", activeConfig.model, activeConfig.endpoint)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", applicationPort),
		Handler: http.HandlerFunc(proxyHandler),
	}

	// Enable HTTP/2 support
	http2.ConfigureServer(server, &http2.Server{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if openTunnel {
		go func() {
			tunnelURL, err := tunnel.CreateCloudflareTunnel(ctx, applicationPort)
			if err != nil {
				log.Printf("Failed to create tunnel: %v", err)
				return
			}
			log.Printf("Public tunnel URL: %s", tunnelURL)
		}()
	} else {
		log.Println("No Tunnel is setup, to use a tunnel, run with the -tunnel flag")
	}

	go func() {
		log.Printf("Starting proxy server on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited properly")
}

func enableCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request: %s %s", r.Method, r.URL.Path)

	if r.Method == http.MethodOptions {
		enableCors(w)
		return
	}

	enableCors(w)

	// Validate API key
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		log.Printf("Missing or invalid Authorization header")
		http.Error(w, "Missing or invalid Authorization header", http.StatusUnauthorized)
		return
	}

	userAPIKey := strings.TrimPrefix(authHeader, "Bearer ")
	if userAPIKey == "" {
		log.Printf("No API key provided")
		http.Error(w, "No API key provided", http.StatusUnauthorized)
		return
	}

	// Handle /v1/models endpoint
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		log.Printf("Handling /v1/models request")
		handleModelsRequest(w)
		return
	}

	// Log headers for debugging
	log.Printf("Request headers: %+v", r.Header)

	// Read and log request body for debugging
	var chatReq ChatRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Error reading request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	if err := json.Unmarshal(body, &chatReq); err != nil {
		log.Printf("Error parsing request JSON: %v", err)
		log.Printf("Raw request body: %s", string(body))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("Parsed request: %+v", chatReq)

	// Handle models endpoint
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		handleModelsRequest(w)
		return
	}

	// Normalize path - support both /v1/chat/completions and /chat/completions
	requestPath := r.URL.Path
	if !strings.HasPrefix(requestPath, "/v1/") {
		// Add /v1/ prefix if not present
		requestPath = "/v1" + requestPath
	}

	// Only handle chat completions
	if requestPath != "/v1/chat/completions" {
		log.Printf("Invalid path: %s (normalized: %s)", r.URL.Path, requestPath)
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	// Restore the body for further reading
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	log.Printf("Request body: %s", string(body))

	// Parse the request to check for streaming - reuse existing chatReq
	if err := json.Unmarshal(body, &chatReq); err != nil {
		log.Printf("Error parsing request JSON: %v", err)
		http.Error(w, "Error parsing request", http.StatusBadRequest)
		return
	}

	log.Printf("Requested model: %s", chatReq.Model)

	originalModel, reasoningEffort, hasThinking := chatReq.ExtractReasoningEffort()

	// Convert to deepseek-chat internally
	chatReq.Model = deepseekChatModel
	log.Printf("Model converted to: %s (original: %s, reasoning effort: %s)", deepseekChatModel, originalModel, reasoningEffort)

	// Convert to DeepSeek request format
	deepseekReq := DeepSeekRequest{
		Model:    deepseekChatModel,
		Messages: convertMessages(chatReq.Messages),
		Stream:   chatReq.Stream,
		Thinking: Thinking{Type: "disabled"},
	}
	if reasoningEffort != ReasoningEffortDefault {
		deepseekReq.ReasoningEffort = reasoningEffort
	}
	if hasThinking {
		deepseekReq.Thinking = Thinking{Type: "enabled"}
	}

	// Copy optional parameters if present
	if chatReq.Temperature != nil {
		deepseekReq.Temperature = *chatReq.Temperature
	}
	if chatReq.MaxTokens != nil {
		deepseekReq.MaxTokens = *chatReq.MaxTokens
	}

	// Handle tools/functions
	if len(chatReq.Tools) > 0 {
		deepseekReq.Tools = chatReq.Tools
		if tc := convertToolChoice(chatReq.ToolChoice); tc != "" {
			deepseekReq.ToolChoice = tc
		}
	} else if len(chatReq.Functions) > 0 {
		// Convert functions to tools format
		tools := make([]Tool, len(chatReq.Functions))
		for i, fn := range chatReq.Functions {
			tools[i] = Tool{
				Type:     "function",
				Function: fn,
			}
		}
		deepseekReq.Tools = tools

		// Convert tool_choice if present
		if tc := convertToolChoice(chatReq.ToolChoice); tc != "" {
			deepseekReq.ToolChoice = tc
		}
	}

	// Create new request body
	modifiedBody, err := json.Marshal(deepseekReq)
	if err != nil {
		log.Printf("Error creating modified request body: %v", err)
		http.Error(w, "Error creating modified request", http.StatusInternalServerError)
		return
	}

	log.Printf("========== Request to DeepSeek ==========")
	if verbose {
		log.Printf("Modified request body: %s", string(modifiedBody))
	} else {
		log.Printf("Modified request body: <verbose logging disabled>")
	}
	log.Printf("==========================================")

	// Create the proxy request to DeepSeek
	targetURL := activeConfig.endpoint + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.Printf("Forwarding to: %s", targetURL)
	proxyReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		log.Printf("Error creating proxy request: %v", err)
		http.Error(w, "Error creating proxy request", http.StatusInternalServerError)
		return
	}

	// Copy headers
	copyHeaders(proxyReq.Header, r.Header)

	// Set DeepSeek API key and content type
	proxyReq.Header.Set("Authorization", "Bearer "+userAPIKey)
	proxyReq.Header.Set("Content-Type", "application/json")
	if chatReq.Stream {
		proxyReq.Header.Set("Accept", "text/event-stream")
	}

	// Add Accept-Language header from request
	if acceptLanguage := r.Header.Get("Accept-Language"); acceptLanguage != "" {
		proxyReq.Header.Set("Accept-Language", acceptLanguage)
	}

	log.Printf("Proxy request headers: %v", proxyReq.Header)

	// Create a custom client with keepalive
	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS:   nil,
		},
		Timeout: 5 * time.Minute,
	}

	// Send the request
	resp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("Error forwarding request: %v", err)
		http.Error(w, "Error forwarding request", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("========== DeepSeek Response ==========")
	log.Printf("Status: %d", resp.StatusCode)
	log.Printf("Headers: %v", resp.Header)

	// Handle error responses
	if resp.StatusCode >= 400 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Error reading error response: %v", err)
			http.Error(w, "Error reading response", http.StatusInternalServerError)
			return
		}
		log.Printf("========== DeepSeek ERROR Response Body ==========")
		log.Printf("%s", string(respBody))
		log.Printf("===================================================")

		// Forward the error response
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// Handle streaming response
	if chatReq.Stream {
		handleStreamingResponse(w, r, resp, originalModel)
		return
	}

	// Handle regular response
	handleRegularResponse(w, resp, originalModel)
}

func handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, originalModel string) {
	log.Printf("Starting streaming response handling with model: %s", originalModel)
	log.Printf("Response status: %d", resp.StatusCode)
	log.Printf("Response headers: %+v", resp.Header)

	// Set headers for streaming response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	// Create a buffered reader for the response body
	reader := bufio.NewReader(resp.Body)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("Warning: ResponseWriter does not support Flush")
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("Error reading stream: %v", err)
			return
		}

		lineStr := string(line)

		// Skip empty lines but forward them for SSE format
		if len(strings.TrimSpace(lineStr)) == 0 {
			w.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		// Handle SSE data lines
		if strings.HasPrefix(lineStr, "data: ") {
			data := strings.TrimPrefix(lineStr, "data: ")
			data = strings.TrimSpace(data)

			// Handle [DONE] marker
			if data == "[DONE]" {
				w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				break
			}

			// Parse and modify the JSON to replace model name
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				// Replace model name with original requested model
				chunk["model"] = originalModel

				// Re-serialize
				modifiedData, err := json.Marshal(chunk)
				if err == nil {
					w.Write([]byte("data: "))
					w.Write(modifiedData)
					w.Write([]byte("\n\n"))
				} else {
					// If re-serialization fails, forward original
					w.Write(line)
				}
			} else {
				// If parsing fails, forward original line
				w.Write(line)
			}
		} else {
			// Forward non-data lines as-is (comments, etc.)
			w.Write(line)
		}

		if flusher != nil {
			flusher.Flush()
		}
	}

	log.Printf("Streaming response completed")
}

func handleRegularResponse(w http.ResponseWriter, resp *http.Response, originalModel string) {
	log.Printf("Handling regular (non-streaming) response")
	log.Printf("Response status: %d", resp.StatusCode)
	log.Printf("Response headers: %+v", resp.Header)

	// Read and log response body
	body, err := readResponse(resp)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		http.Error(w, "Error reading response from upstream", http.StatusInternalServerError)
		return
	}

	log.Printf("Original response body: %s", string(body))

	// Parse the DeepSeek response
	var deepseekResp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &deepseekResp); err != nil {
		log.Printf("Error parsing DeepSeek response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Convert to OpenAI format
	openAIResp := struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}{
		ID:      deepseekResp.ID,
		Object:  "chat.completion",
		Created: deepseekResp.Created,
		Model:   originalModel,
		Usage:   deepseekResp.Usage,
	}

	// Convert choices and ensure tool calls are properly handled
	openAIResp.Choices = make([]struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}, len(deepseekResp.Choices))

	for i, choice := range deepseekResp.Choices {
		openAIResp.Choices[i] = struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{
			Index:        choice.Index,
			Message:      choice.Message,
			FinishReason: choice.FinishReason,
		}

		// Ensure tool calls are properly formatted in the message
		if len(choice.Message.ToolCalls) > 0 {
			log.Printf("Processing %d tool calls in choice %d", len(choice.Message.ToolCalls), i)
			for j, tc := range choice.Message.ToolCalls {
				log.Printf("Tool call %d: %+v", j, tc)
				// Ensure the tool call has the required fields
				if tc.Function.Name == "" {
					log.Printf("Warning: Empty function name in tool call %d", j)
					continue
				}
				// Keep the tool call as is since it's already in the correct format
				openAIResp.Choices[i].Message.ToolCalls = append(openAIResp.Choices[i].Message.ToolCalls, tc)
			}
		}
	}

	// Convert back to JSON
	modifiedBody, err := json.Marshal(openAIResp)
	if err != nil {
		log.Printf("Error creating modified response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	log.Printf("Modified response body: %s", string(modifiedBody))

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(modifiedBody)
	log.Printf("Modified response sent successfully")
}

func copyHeaders(dst, src http.Header) {
	// Headers to skip
	skipHeaders := map[string]bool{
		"Content-Length":    true,
		"Content-Encoding":  true,
		"Transfer-Encoding": true,
		"Connection":        true,
	}

	for k, vv := range src {
		if !skipHeaders[k] {
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}
}

func handleModelsRequest(w http.ResponseWriter) {
	log.Printf("Handling models request")

	// Get the requested model from the query parameters
	response := ModelsResponse{
		Object: "list",
		Data:   []Model{},
	}
	for reasoningEffort := range reasoningEffortMap {
		id := deepseekChatModel
		if reasoningEffort != ReasoningEffortDefault {
			id = fmt.Sprintf("%s$%s", deepseekChatModel, reasoningEffort)
		}
		response.Data = append(response.Data, Model{
			ID:      id,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "deepseek",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	log.Printf("Models response sent successfully")
}

func readResponse(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("error creating gzip reader: %v", err)
		}
		defer gzReader.Close()
		reader = gzReader
	case "br":
		reader = brotli.NewReader(resp.Body)
	case "deflate":
		reader = flate.NewReader(resp.Body)
	}

	return io.ReadAll(reader)
}
