package main

import (
        "bufio"
        "bytes"
        "compress/flate"
        "compress/gzip"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "net/http"
        "os"
        "strings"
        "time"

        "github.com/andybalholm/brotli"
        "github.com/joho/godotenv"
        "golang.org/x/net/http2"
)

const (
        openRouterEndpoint = "https://openrouter.ai/api/v1"
        deepseekChatModel = "deepseek/deepseek-chat"
)

var openRouterAPIKey string

func init() {
        // Load .env file
        if err := godotenv.Load(); err != nil {
                log.Printf("Warning: .env file not found or error loading it: %v", err)
        }

        // Get OpenRouter API key
        openRouterAPIKey = os.Getenv("OPENROUTER_API_KEY")
        if openRouterAPIKey == "" {
                log.Fatal("OPENROUTER_API_KEY environment variable is required")
        }
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

type Message struct {
        Role       string     `json:"role"`
        Content    string     `json:"content"`
        ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
        ToolCallID string     `json:"tool_call_id,omitempty"`
        Name       string     `json:"name,omitempty"`
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

                // Convert function role to tool role
                if msg.Role == "function" {
                        converted[i].Role = "tool"
                        log.Printf("Converted function role to tool role")
                }

                // Handle assistant messages with tool calls
                if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
                        log.Printf("Processing assistant message with %d tool calls", len(msg.ToolCalls))

                        // Ensure tool calls are properly formatted
                        toolCalls := make([]ToolCall, len(msg.ToolCalls))
                        for j, tc := range msg.ToolCalls {
                                toolCalls[j] = ToolCall{
                                        ID:   tc.ID,
                                        Type: "function",
                                        Function: struct {
                                                Name      string `json:"name"`
                                                Arguments string `json:"arguments"`
                                        }{
                                                Name:      tc.Function.Name,
                                                Arguments: tc.Function.Arguments,
                                        },
                                }
                                log.Printf("Processed tool call %d - Name: %s", j, tc.Function.Name)
                        }
                        converted[i].ToolCalls = toolCalls
                }

                // Handle tool response messages
                if msg.Role == "tool" || msg.Role == "function" {
                        log.Printf("Processing tool/function response message")
                        converted[i].Role = "tool"
                        if msg.Name != "" {
                                log.Printf("Tool response from function: %s", msg.Name)
                        }
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
type DeepSeekRequest struct {
        Model       string    `json:"model"`
        Messages    []Message `json:"messages"`
        Stream      bool      `json:"stream"`
        Temperature float64   `json:"temperature,omitempty"`
        MaxTokens   int       `json:"max_tokens,omitempty"`
        Tools       []Tool    `json:"tools,omitempty"`
        ToolChoice  string    `json:"tool_choice,omitempty"`
}

func main() {
        log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

        server := &http.Server{
                Addr:    ":9000",
                Handler: http.HandlerFunc(proxyHandler),
        }

        // Enable HTTP/2 support
        http2.ConfigureServer(server, &http2.Server{})

        log.Printf("Starting proxy server on %s", server.Addr)
        if err := server.ListenAndServe(); err != nil {
                log.Fatalf("Server failed: %v", err)
        }
}

func enableCors(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
        w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
        w.Header().Set("Access-Control-Allow-Credentials", "true")
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
        log.Printf("Received request: %s %s", r.Method, r.URL.Path)

        if r.Method == "OPTIONS" {
                enableCors(w, r)
                return
        }

        enableCors(w, r)

        // Validate API key
        authHeader := r.Header.Get("Authorization")
        if !strings.HasPrefix(authHeader, "Bearer ") {
                log.Printf("Missing or invalid Authorization header")
                http.Error(w, "Missing or invalid Authorization header", http.StatusUnauthorized)
                return
        }

        userAPIKey := strings.TrimPrefix(authHeader, "Bearer ")
        if userAPIKey != openRouterAPIKey {
                log.Printf("Invalid API key provided")
                http.Error(w, "Invalid API key", http.StatusUnauthorized)
                return
        }

        // Handle /v1/models endpoint
        if r.URL.Path == "/v1/models" && r.Method == "GET" {
                log.Printf("Handling /v1/models request")
                handleModelsRequest(w)
                return
        }

        // Log headers for debugging
        log.Printf("Request headers: %+v", r.Header)

        // Only handle chat completions API requests
        if r.URL.Path != "/v1/chat/completions" {
                log.Printf("Invalid path: %s", r.URL.Path)
                http.Error(w, "Only /v1/chat/completions endpoint is supported", http.StatusNotFound)
                return
        }

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
        if r.URL.Path == "/v1/models" {
                handleModelsRequest(w)
                return
        }

        // Only handle API requests with /v1/ prefix
        if !strings.HasPrefix(r.URL.Path, "/v1/") {
                log.Printf("Invalid path: %s", r.URL.Path)
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

        // Store original model name for response
        originalModel := chatReq.Model

        // Convert to deepseek-chat internally
        chatReq.Model = deepseekChatModel
        log.Printf("Model converted to: %s (original: %s)", deepseekChatModel, originalModel)

        // Convert to DeepSeek request format
        deepseekReq := DeepSeekRequest{
                Model:    deepseekChatModel,
                Messages: convertMessages(chatReq.Messages),
                Stream:   chatReq.Stream,
        }

        // Set default temperature if not provided
        if chatReq.Temperature != nil {
                deepseekReq.Temperature = *chatReq.Temperature
        } else {
                defaultTemp := 0.7
                deepseekReq.Temperature = defaultTemp
        }

        // Set default max tokens if not provided
        if chatReq.MaxTokens != nil {
                deepseekReq.MaxTokens = *chatReq.MaxTokens
        } else {
                defaultMaxTokens := 4096
                deepseekReq.MaxTokens = defaultMaxTokens
        }

        // Handle tools and tool choice
        if len(chatReq.Tools) > 0 {
                deepseekReq.Tools = chatReq.Tools
                deepseekReq.ToolChoice = convertToolChoice(chatReq.ToolChoice)
        } else if len(chatReq.Functions) > 0 {
                // Convert legacy functions to tools
                tools := make([]Tool, len(chatReq.Functions))
                for i, fn := range chatReq.Functions {
                        tools[i] = Tool{
                                Type:     "function",
                                Function: fn,
                        }
                }
                deepseekReq.Tools = tools
                deepseekReq.ToolChoice = convertToolChoice(chatReq.ToolChoice)
        }

        // Create new request body
        modifiedBody, err := json.Marshal(deepseekReq)
        if err != nil {
                log.Printf("Error creating modified request body: %v", err)
                http.Error(w, "Error creating modified request", http.StatusInternalServerError)
                return
        }

        log.Printf("Modified request body: %s", string(modifiedBody))

        // Create the proxy request to OpenRouter
        targetURL := openRouterEndpoint + "/chat/completions"
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

        // Set OpenRouter API key and required headers
        proxyReq.Header.Set("Authorization", "Bearer "+openRouterAPIKey)
        proxyReq.Header.Set("Content-Type", "application/json")
        proxyReq.Header.Set("HTTP-Referer", "https://github.com/danilofalcao/cursor-deepseek") // Optional, for OpenRouter rankings
        proxyReq.Header.Set("X-Title", "Cursor DeepSeek") // Optional, for OpenRouter rankings
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
                // Remove global timeout as we'll handle timeouts per request type
                Timeout: 0,
        }

        // Create context with timeout based on streaming
        ctx := context.Background()
        if !chatReq.Stream {
                // Use timeout only for non-streaming requests
                var cancel context.CancelFunc
                ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
                defer cancel()
        }

        // Create the request with context
        proxyReq = proxyReq.WithContext(ctx)

        // Send the request
        resp, err := client.Do(proxyReq)
        if err != nil {
                log.Printf("Error forwarding request: %v", err)
                http.Error(w, "Error forwarding request", http.StatusBadGateway)
                return
        }
        defer resp.Body.Close()

        log.Printf("OpenRouter response status: %d", resp.StatusCode)
        log.Printf("OpenRouter response headers: %v", resp.Header)

        // Handle error responses
        if resp.StatusCode >= 400 {
                respBody, err := io.ReadAll(resp.Body)
                if err != nil {
                        log.Printf("Error reading error response: %v", err)
                        http.Error(w, "Error reading response", http.StatusInternalServerError)
                        return
                }
                log.Printf("OpenRouter error response: %s", string(respBody))

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
                handleStreamingResponse(w, resp)
                return
        }

        // Handle regular response
        handleRegularResponse(w, resp)
}

func handleStreamingResponse(w http.ResponseWriter, resp *http.Response) {
        log.Printf("Starting streaming response handling")

        // Set headers for streaming response
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        w.WriteHeader(resp.StatusCode)

        // Create a buffered reader for the response body
        reader := bufio.NewReaderSize(resp.Body, 1024)

        // Create a context that will be cancelled when the client disconnects
        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        // Create a channel to detect client disconnection
        clientGone := w.(http.CloseNotifier).CloseNotify()

        // Create a channel for errors
        errChan := make(chan error, 1)

        // Start processing in a goroutine
        go func() {
                defer close(errChan)
                for {
                        select {
                        case <-ctx.Done():
                                return
                        case <-clientGone:
                                log.Printf("Client connection closed")
                                cancel()
                                return
                        default:
                                // Read until we get a complete SSE message
                                var buffer bytes.Buffer
                                for {
                                        line, err := reader.ReadBytes('\n')
                                        if err != nil {
                                                if err == io.EOF {
                                                        log.Printf("EOF reached")
                                                        return
                                                }
                                                log.Printf("Error reading from response: %v", err)
                                                errChan <- err
                                                return
                                        }

                                        // Log the received line for debugging
                                        log.Printf("Received line: %s", string(line))

                                        // Write to buffer
                                        buffer.Write(line)

                                        // If we've reached the end of an event (double newline)
                                        if bytes.HasSuffix(buffer.Bytes(), []byte("\n\n")) {
                                                break
                                        }
                                }

                                // Get the complete message
                                message := buffer.Bytes()

                                // Skip if empty
                                if len(bytes.TrimSpace(message)) == 0 {
                                        continue
                                }

                                // Write the message
                                if _, err := w.Write(message); err != nil {
                                        log.Printf("Error writing to client: %v", err)
                                        errChan <- err
                                        return
                                }

                                // Flush after each complete message
                                if f, ok := w.(http.Flusher); ok {
                                        f.Flush()
                                        log.Printf("Flushed message to client")
                                }
                        }
                }
        }()

        // Wait for completion or error
        select {
        case err := <-errChan:
                if err != nil {
                        log.Printf("Error in streaming response: %v", err)
                }
        case <-clientGone:
                log.Printf("Client disconnected")
        case <-ctx.Done():
                log.Printf("Context cancelled")
        }

        log.Printf("Streaming response handler completed")
}

func handleRegularResponse(w http.ResponseWriter, resp *http.Response) {
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
        var deepseekResp map[string]interface{}
        if err := json.Unmarshal(body, &deepseekResp); err != nil {
                log.Printf("Error parsing DeepSeek response: %v", err)
                w.WriteHeader(http.StatusInternalServerError)
                return
        }

        // Use the original model name instead of hardcoding gpt-4o
        originalModel := deepseekResp["model"].(string)
        deepseekResp["model"] = originalModel

        // Process choices to ensure tool calls are properly formatted
        if choices, ok := deepseekResp["choices"].([]interface{}); ok {
                for _, choice := range choices {
                        if choiceMap, ok := choice.(map[string]interface{}); ok {
                                if message, ok := choiceMap["message"].(map[string]interface{}); ok {
                                        // Handle tool calls in the message
                                        if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
                                                for i, tc := range toolCalls {
                                                        if tcMap, ok := tc.(map[string]interface{}); ok {
                                                                // Ensure type is set to "function"
                                                                tcMap["type"] = "function"

                                                                // Ensure function field is properly formatted
                                                                if fn, ok := tcMap["function"].(map[string]interface{}); ok {
                                                                        // Make sure required fields exist
                                                                        if _, ok := fn["name"]; !ok {
                                                                                fn["name"] = ""
                                                                        }
                                                                        if _, ok := fn["arguments"]; !ok {
                                                                                fn["arguments"] = "{}"
                                                                        }
                                                                }
                                                        }
                                                        toolCalls[i] = tc
                                                }
                                                message["tool_calls"] = toolCalls
                                        }
                                }
                        }
                }
        }

        // Convert back to JSON
        modifiedBody, err := json.Marshal(deepseekResp)
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

        response := ModelsResponse{
                Object: "list",
                Data: []Model{
                        {
                                ID:      deepseekChatModel,
                                Object:  "model",
                                Created: time.Now().Unix(),
                                OwnedBy: "deepseek",
                        },
                },
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