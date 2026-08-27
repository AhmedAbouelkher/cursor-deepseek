# DeepSeek API Proxy

A high-performance HTTP/2-enabled proxy server designed specifically to enable Cursor IDE's Composer to use DeepSeek's, OpenRouter's and Ollama's language models. This proxy translates OpenAI-compatible API requests to DeepSeek/OpenRouter/Ollama API format, allowing Cursor's Composer and other OpenAI API-compatible tools to seamlessly work with these models.

## Primary Use Case

This proxy was created to enable Cursor IDE users to leverage DeepSeek's, OpenRouter's and Ollama's powerful language models through Cursor's Composer interface as an alternative to OpenAI's models. By running this proxy locally, you can configure Cursor's Composer to use these models for AI assistance, code generation, and other AI features. It handles all the necessary request/response translations and format conversions to make the integration seamless.

## Features

- HTTP/2 support for improved performance
- Full CORS support
- Streaming responses
- Support for function calling/tools
- Automatic message format conversion
- Compression support (Brotli, Gzip, Deflate)
- Compatible with OpenAI API client libraries
- API key validation for secure access
- Docker container support with multi-variant builds (deepseek, openrouter, ollama)
- Built-in Cloudflare Quick Tunnel (`-tunnel`) — no external binary required
- DeepSeek reasoning effort control (`deepseek-chat$low`, `$high`, `$max`)
- DeepSeek thinking mode (e.g. `deepseek-chat$high$thinking`)
- DeepSeek Coder model support (`-model coder`)
- Graceful shutdown

## Prerequisites

- Cursor Pro Subscription
- Go 1.26 or higher
- DeepSeek API key (required for `proxy.go`)
- OpenRouter API key or local Ollama server (for the corresponding variants)

## Installation

1. Clone the repository
2. Install dependencies:

```bash
go mod download
```

### Docker Installation

The proxy supports both DeepSeek and OpenRouter variants. Choose the appropriate build command for your needs:

1. Build the Docker image:
   - For DeepSeek (default):

   ```bash
   docker build -t cursor-deepseek .
   ```

   - For OpenRouter:

   ```bash
   docker build -t cursor-openrouter --build-arg PROXY_VARIANT=openrouter .
   ```

   - For Ollama:

   ```bash
   docker build -t cursor-ollama --build-arg PROXY_VARIANT=ollama .
   ```

2. Configure environment variables:
   - Copy the example configuration:

   ```bash
   cp .env.example .env
   ```

   - Edit `.env` and add your API key (either DeepSeek or OpenRouter)

3. Run the container:

```bash
docker run -p 9000:9000 --env-file .env cursor-deepseek
# OR for OpenRouter
docker run -p 9000:9000 --env-file .env cursor-openrouter
# OR for Ollama
docker run -p 9000:9000 --env-file .env cursor-ollama
```

## Configuration

The repository includes an `.env.example` file showing the required environment variables. To configure:

1. Copy the example configuration:

```bash
cp .env.example .env
```

1. Edit `.env` and add your API key:

```bash
# For DeepSeek
DEEPSEEK_API_KEY=your_deepseek_api_key_here

# OR for OpenRouter
OPENROUTER_API_KEY=your_openrouter_api_key_here

# OR for Ollama
OLLAMA_API_KEY=your_ollama_api_key_here
```

Note: Only configure ONE of the API keys based on which variant you're using.

## Usage

1. Start the proxy server:

```bash
go run proxy.go
# Specify the DeepSeek model variant:
go run proxy.go -model chat   # deepseek-chat (default)
go run proxy.go -model coder  # deepseek-coder (beta endpoint)
# Change the port:
go run proxy.go -port 8080
# Open a public Cloudflare Quick Tunnel automatically:
go run proxy.go -tunnel
# OR for OpenRouter
go run openrouter/proxy-openrouter.go
# OR for Ollama
go run ollama/proxy-ollama.go
```

The server will start on port 9000 by default.

1. Use the proxy with your OpenAI API clients by setting the base URL to `http://your-public-endpoint:9000/v1`

## Exposing the Endpoint Publicly

The proxy includes a built-in Cloudflare Quick Tunnel, so you can expose your local server without installing anything.

### Built-in Cloudflare Quick Tunnel

Start the proxy with the `-tunnel` flag:

```bash
go run proxy.go -tunnel
```

A random public URL (e.g. `https://random-subdomain.trycloudflare.com`) is created automatically and logged at startup.

Use this URL as your OpenAI API base URL in Cursor's settings:

```
https://random-subdomain.trycloudflare.com/v1
```

### Alternatives

You can also expose your local proxy server to the internet using ngrok or similar services. This is useful when you need to access the proxy from external applications or different networks.

#### Using ngrok

1. Install ngrok from <https://ngrok.com/download>

2. Start your proxy server locally (it will run on port 9000)

3. In a new terminal, run ngrok:

```bash
ngrok http 9000
```

1. ngrok will provide you with a public URL (e.g., <https://your-unique-id.ngrok.io>)

2. Use this URL as your OpenAI API base URL in Cursor's settings:

```
https://your-unique-id.ngrok.io/v1
```

#### Other Services

You can also use other services to expose your endpoint:

1. **Cloudflare Tunnel**:
   - Install cloudflared
   - Run: `cloudflared tunnel --url http://localhost:9000`

2. **LocalTunnel**:
   - Install: `npm install -g localtunnel`
   - Run: `lt --port 9000`

Remember to always secure your endpoint appropriately when exposing it to the internet.

### Supported Endpoints

- `/v1/chat/completions` - Chat completions endpoint
- `/v1/models` - Models listing endpoint

### Model Mapping

The proxy maps every requested model to `deepseek-chat` internally and rewrites the model name in responses back to what you requested, so any model name you pick in Cursor works.

- `deepseek-chat` — DeepSeek's native chat model (default)
- `deepseek-coder` — DeepSeek Coder, via `-model coder` (beta endpoint)
- `deepseek-chat$low` / `$high` / `$max` — DeepSeek reasoning effort control
- `deepseek-chat$high$thinking` — enables DeepSeek thinking mode

`/v1/models` returns the available variants: `deepseek-chat`, `deepseek-chat$low`, `deepseek-chat$high`, and `deepseek-chat$max`.

## Dependencies

- `github.com/andybalholm/brotli` - Brotli compression support
- `github.com/joho/godotenv` - Environment variable management
- `golang.org/x/net` - HTTP/2 support
- `github.com/cloudflare/cloudflared` - Embedded Cloudflare Quick Tunnel
- `github.com/google/uuid` - UUID parsing for tunnel credentials
- `github.com/pkg/errors` - Error wrapping

## Security

- The proxy includes CORS headers for cross-origin requests
- API keys are required and validated against environment variables
- Secure handling of request/response data
- Strict API key validation for all requests
- HTTPS support through HTTP/2
- Environment variables are never committed to the repository

## License

This project is licensed under the GNU General Public License v2.0 (GPLv2). See the [LICENSE.md](LICENSE.md) file for details.
