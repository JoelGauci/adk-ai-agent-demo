# Google ADK AI Agent Demo 1

<p align="center">
  <img src="https://miro.medium.com/v2/resize:fit:1024/format:webp/1*A4-k_sI5kmrphjS4tJ_rpA.png" alt="Google ADK Logo" width="300">
</p>

This project demonstrates an autonomous AI Agent built with Google's **Agent Development Kit (ADK)** for Go. The agent connects to specialized backend platforms (Lego and Robot diagnostics) via the **Model Context Protocol (MCP)** over Streamable transport.

Apigee (X or Hybrid) serves as the **Extended Agent Gateway**, routing traffic to the MCP servers, and as the **Identity Facade** for token generation and exchange.

The service orchestrates isolated user-agent instances and utilizes a high-performance streaming architecture using Server-Sent Events to provide users dynamic, narrative explanations alongside detailed real-time system logs.

## Prerequisites
Before installing and deploying, please ensure you have following resources available:
- **Go 1.23+** installed.
- **Apigee Gateway (X or Hybrid)**: Acting as the Extended Agent Gateway.
- **Lego MCP Server**: Accessible via Apigee (e.g., `https://[AGENT_GATEWAY_HOST]/mcp/lego`). Based on the [Lego Builder API](https://github.com/JoelGauci/lego-builder).
- **Robot MCP Server**: Accessible via Apigee (e.g., `https://[AGENT_GATEWAY_HOST]/mcp/robot`). Based on the [Robot API](https://github.com/JoelGauci/robot-api).
- **OAuth Identity Provider / Apigee Identity Facade**: Configured to support Authorization Code with PKCE and Token Exchange (`urn:ietf:params:oauth:grant-type:token-exchange`). A reference implementation is available in the [Apigee Devrel repository](https://github.com/apigee/devrel/tree/main/references/identity-facade).
- **Google Cloud Account**: With permissions to deploy onto Cloud Run and access Gemini APIs.

### Creating MCP Servers on Apigee
The MCP servers (Lego and Robot) can be created in two ways:
- Using **apigee-go-gen** ([MCP rendering](https://apigee.github.io/apigee-go-gen/render/mcp/)) to generate MCP proxies from OpenAPI Specs (OAS).
- Using the **native MCP support in Apigee X** ([Apigee MCP Overview](https://docs.cloud.google.com/apigee/docs/api-platform/apigee-mcp/apigee-mcp-overview)) to create a Discovery proxy from OAS.

In both cases, OpenAPI specifications (OAS) are used to create the proxies (MCP proxies for Apigee go-gen or a Discovery proxy for MCP in Apigee X).

## Environment Variables Configuration
The agent is strictly configured via environment. Do not commit secrets into repositories. Configure these values inside your deployment manager (e.g., Cloud Secret Manager or Local `.env` simulation):

> [!TIP]
> **Security Best Practice (Recommended)**: Instead of using a static `GOOGLE_API_KEY` (Gemini Developer API), it is highly recommended to authenticate using a **Service Account** with **Application Default Credentials (ADC)** via **Vertex AI**.
> This eliminates the need to manage static API keys and leverages Google Cloud's native IAM security.
> To use this mode:
> 1. Leave `GOOGLE_API_KEY` empty/unset.
> 2. Set `GOOGLE_GENAI_USE_VERTEXAI="true"`.
> 3. Ensure the service account running the application (e.g., the default Compute Engine service account on Cloud Run) has the **Vertex AI User** (`roles/aiplatform.user`) role.

> [!NOTE]
> The placeholder `[AGENT_GATEWAY_HOST]` used in the examples below refers to your **Apigee Gateway host** (X or Hybrid).

| Variable | Description | Example |
|----------|-------------|---------|
| `PORT` | Port to run application on | `8080` |
| `GOOGLE_API_KEY` | Gemini API Key (Optional if using Service Account/ADC) | `AIzaSy...` |
| `OAUTH_CLIENT_ID` | Application Client Identity | `xkey-123456789` |
| `OAUTH_CLIENT_SECRET` | Secret for server-to-server token delivery | `xsecret_sensitive` |
| `OAUTH_AUTHORIZE_URL` | Authorization endpoint for user browser redirection | `https://[AGENT_GATEWAY_HOST]/v1/oauth20/authorize` |
| `OAUTH_TOKEN_URL` | Endpoint used for base delivery and subsequent exchanges | `https://[AGENT_GATEWAY_HOST]/v1/oauth20/token` |
| `OAUTH_REDIRECT_URI` | Return redirect must match provider config exactly | `http://localhost:8080/callback` or `https://your-service-hash.a.run.app/callback` |
| `LEGO_MCP_ENDPOINT` | The URL pointing to Lego MCP Server | `https://[AGENT_GATEWAY_HOST]/mcp/lego` |
| `ROBOT_MCP_ENDPOINT` | The URL pointing to Robot MCP Server | `https://[AGENT_GATEWAY_HOST]/mcp/robot` |
| `GOOGLE_GENAI_USE_VERTEXAI` | Set to `true` to use Vertex AI backend instead of Gemini Developer API | `true` |
| `GOOGLE_CLOUD_PROJECT` | GCP Project ID (Required for Vertex AI / Service Account mode) | `apigee-x-jog` |
| `GOOGLE_CLOUD_LOCATION` | GCP Region/Location for Vertex AI (e.g., `us-central1` or `europe-west3`) | `us-central1` |

## Project Structure

The project is organized with the following directory structure to ensure clean separation of concerns:

- `cmd/demo/`: Contains the main entry point (`main.go`) and the web interface (`index.html`).
- `internal/agent/`: Contains the logic for building and configuring the AI agent.
- `internal/auth/`: Contains OAuth2 and Token Exchange logic, as well as custom HTTP transports.

## Compilation & Execution

### 1. Prerequisites
Ensure you have Go 1.23+ installed.

### 2. Retrieve Dependencies
Navigate to the project root and run:
```bash
go mod tidy
```

### 3. Compilation
To compile the Go code, run the following command from the project root:
```bash
go build -o demo-agent ./cmd/demo
```
This command compiles the code in `cmd/demo` (including the embedded `index.html`) and creates an executable named `demo-agent` in the current directory.

### 4. Local Execution
Define export variables in your current shell:
```bash
export OAUTH_REDIRECT_URI="http://localhost:8080/callback"
export PORT="8080"
# (Export remaining fields from environment variables configuration table above)
```
Launch the application binary:
```bash
./demo-agent
```
Navigate your browser to `http://localhost:8080` to initiate the authentication workflow.

### 5. Sign Out & Re-initialization
To clear user sessions, click the **Sign Out** button in the navigation header. This action deletes the `base_token` and `user_email` cookies from the browser, calls the backend to remove the active token association from the server-side registry, and resets the interface, requiring a new authentication workflow.

## Deployment to Google Cloud Run

For the specific target project `apigee-x-jog` within the region `europe-west1`, proceed with the automatic container build and deployment steps:

### Step A: Setup Sensitive Assets inside Secret Manager
Best practice dictates feeding sensitive keys securely.
```bash
gcloud secrets create adk-oauth-secret --data-file="secret.txt" --project=apigee-x-jog
```

### Step B: Containerize and Deploy Command
Execute this direct `gcloud run deploy` from source code root directory.

#### Option 1: Using Service Account (Recommended Best Practice)
Deploy using the service account's Application Default Credentials (ADC) via Vertex AI:
```bash
gcloud run deploy adk-ai-agent-demo \
  --source . \
  --project=apigee-x-jog \
  --region=europe-west1 \
  --allow-unauthenticated \
  --set-env-vars="GOOGLE_GENAI_USE_VERTEXAI=true" \
  --set-env-vars="GOOGLE_CLOUD_PROJECT=apigee-x-jog" \
  --set-env-vars="GOOGLE_CLOUD_LOCATION=us-central1" \
  --set-env-vars="OAUTH_CLIENT_ID=xkey-123456789" \
  --set-env-vars="OAUTH_AUTHORIZE_URL=https://[AGENT_GATEWAY_HOST]/v1/oauth20/authorize" \
  --set-env-vars="OAUTH_TOKEN_URL=https://[AGENT_GATEWAY_HOST]/v1/oauth20/token" \
  --set-env-vars="LEGO_MCP_ENDPOINT=https://[AGENT_GATEWAY_HOST]/mcp/lego" \
  --set-env-vars="ROBOT_MCP_ENDPOINT=https://[AGENT_GATEWAY_HOST]/mcp/robot" \
  --set-env-vars="OAUTH_REDIRECT_URI=https://[SERVICE_FINAL_URL]/callback" \
  --set-secrets="OAUTH_CLIENT_SECRET=adk-oauth-secret:latest"
```
*Note: Make sure to grant the **Vertex AI User** (`roles/aiplatform.user`) role to the Cloud Run service account (e.g., `[PROJECT_NUMBER]-compute@developer.gserviceaccount.com`).*

#### Option 2: Using API Key (Legacy)
```bash
gcloud run deploy adk-ai-agent-demo \
  --source . \
  --project=apigee-x-jog \
  --region=europe-west1 \
  --allow-unauthenticated \
  --set-env-vars="GOOGLE_API_KEY=PASTE_API_KEY_HERE" \
  --set-env-vars="OAUTH_CLIENT_ID=xkey-123456789" \
  --set-env-vars="OAUTH_AUTHORIZE_URL=https://[AGENT_GATEWAY_HOST]/v1/oauth20/authorize" \
  --set-env-vars="OAUTH_TOKEN_URL=https://[AGENT_GATEWAY_HOST]/v1/oauth20/token" \
  --set-env-vars="LEGO_MCP_ENDPOINT=https://[AGENT_GATEWAY_HOST]/mcp/lego" \
  --set-env-vars="ROBOT_MCP_ENDPOINT=https://[AGENT_GATEWAY_HOST]/mcp/robot" \
  --set-env-vars="OAUTH_REDIRECT_URI=https://[SERVICE_FINAL_URL]/callback" \
  --set-secrets="OAUTH_CLIENT_SECRET=adk-oauth-secret:latest"
```
*Note: First deployment might fail on auth redirection until you define `OAUTH_REDIRECT_URI` with the generated Cloud Run URL then redeploy.*

## Architecture Highlights

1. **Multi-Tenant Token Scoping**: Tokens fetched via Token Exchange are securely stored inside a concurrent-safe hash map segmenting credentials strictly by User Hash + Audience Pair preventing any lateral account leakage.
2. **Memory Leak Resilience**: Standard `mcp.StreamableClientTransport` creates background goroutines tied indefinitely to active streams. This project custom-developed a Proxy-Layer `CloseableTransport` pattern. When `handleChat` returns, strict `io.Closer` deferrals are triggered, fully destroying orphans and preserving long-running runtime health.
3. **Thread-Safe Event Multicasting**: All generated AI content and telemetry logs are funneled onto channels merging into a single strictly defined loop, eradicating any `http.ResponseWriter` concurrency crashes.
4. **Zero-Trust Server Side Identity Mapping**: Implements a secure registry binding verifiable user profiles to unguessable server-issued tokens, completely neutralizing client-side spoofing or hijacking attempts.
5. **Canonical Deduplication**: Employs localized state buffers to ensure clean logic filtering, preventing content delivery overlap between incremental fragments and consolidated stream outputs.
6. **Apigee Integration**: Leverages Apigee (X or Hybrid) as the Extended Agent Gateway and Identity Facade, ensuring secure token exchange and routing.
