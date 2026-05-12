package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"

	"adk-ai-agent-demo-1/internal/auth"
)

// CloseableTransport acts as an interceptor ensuring that background goroutines
// created by hidden lazy connections are fully collectable on dispose.
type CloseableTransport struct {
	underlying mcp.Transport
	mu         sync.Mutex
	conns      []mcp.Connection
}

// NewCloseableTransport creates a new CloseableTransport.
func NewCloseableTransport(t mcp.Transport) *CloseableTransport {
	return &CloseableTransport{underlying: t}
}

// Connect establishes a connection and tracks it for later cleanup.
func (c *CloseableTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := c.underlying.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.conns = append(c.conns, conn)
	c.mu.Unlock()
	return conn, nil
}

// Close closes all tracked connections.
func (c *CloseableTransport) Close() error {
	c.mu.Lock()
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()

	var errs []error
	for _, cn := range conns {
		if err := cn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// BuildUserAgent constructs an isolated ADK Agent while properly wrapping transports
// and returning an aggregated slice of closers to allow strict life-cycle management.
func BuildUserAgent(ctx context.Context, geminiAPIKey string, baseToken string) (agent.Agent, []io.Closer, error) {
	var closers []io.Closer

	// 1. Initialize Gemini model.
	geminiModel, err := gemini.NewModel(ctx, "gemini-2.5-flash", &genai.ClientConfig{
		APIKey: geminiAPIKey,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed initializing gemini: %w", err)
	}

	// 2. Lego Setup
	legoAuth := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &auth.McpAuthTransport{
			Base:      http.DefaultTransport,
			Audience:  auth.LegoAudience,
			BaseToken: baseToken,
		},
	}
	rawLego := &mcp.StreamableClientTransport{
		Endpoint:             auth.LegoEndpoint,
		HTTPClient:           legoAuth,
		DisableStandaloneSSE: true,
	}
	wrappedLego := NewCloseableTransport(rawLego)
	closers = append(closers, wrappedLego)

	// 3. Robot Setup
	robotAuth := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &auth.McpAuthTransport{
			Base:      http.DefaultTransport,
			Audience:  auth.RobotAudience,
			BaseToken: baseToken,
		},
	}
	rawRobot := &mcp.StreamableClientTransport{
		Endpoint:             auth.RobotEndpoint,
		HTTPClient:           robotAuth,
		DisableStandaloneSSE: true,
	}
	wrappedRobot := NewCloseableTransport(rawRobot)
	closers = append(closers, wrappedRobot)

	// 4. Tools creation
	legoTS, err := mcptoolset.New(mcptoolset.Config{
		Transport: wrappedLego,
	})
	if err != nil {
		wrappedLego.Close()
		wrappedRobot.Close()
		return nil, nil, fmt.Errorf("lego toolset creation crash: %w", err)
	}

	robotTS, err := mcptoolset.New(mcptoolset.Config{
		Transport: wrappedRobot,
	})
	if err != nil {
		wrappedLego.Close()
		wrappedRobot.Close()
		return nil, nil, fmt.Errorf("robot toolset creation crash: %w", err)
	}

	// 5. Root agent construction
	a, err := llmagent.New(llmagent.Config{
		Name:        "mcp_orchestrator",
		Description: "Expert capability broker utilizing LEGO status indicators and ROBOT instrumentation frameworks.",
		Instruction: `
			<directives>
			- MANDATE: You MUST invoke available LEGO and ROBOT tools to fulfill ANY operational command, state-change, or status check. Actionable intent explicitly overrides all metadata exceptions.
			- RESTRICTION: You CANNOT answer environment-specific or task-oriented questions without first activating an actual tool call.
			- METADATA EXCEPTION: Authorized ONLY for pure high-level inquiries requesting a general list or abstract inventory of pre-loaded tools. 
			- FORMATTING: ALWAYS present output as narrative summaries using clean paragraphs. NEVER display raw JSON schemas.
			- LANGUAGE: ALWAYS respond in the same language used in the current prompt.
			</directives>
		`,
		Model:    geminiModel,
		Toolsets: []tool.Toolset{legoTS, robotTS},

		GenerateContentConfig: &genai.GenerateContentConfig{},

		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(callbackCtx agent.CallbackContext, req *model.LLMRequest) (*model.LLMResponse, error) {
				if req == nil {
					return nil, nil
				}

				var logger auth.LogFunc
				if l, ok := callbackCtx.Value(auth.TransportLoggerKey{}).(auth.LogFunc); ok {
					logger = l
				}

				dispatchLog := func(msg string) {
					if logger != nil {
						logger("AUDIT", msg)
					}
				}

				dispatchLog(fmt.Sprintf("Active Tools Pack count: %d", len(req.Tools)))
				if req.Config != nil && req.Config.Tools != nil {
					for _, t := range req.Config.Tools {
						if t == nil {
							continue
						}
						for _, fd := range t.FunctionDeclarations {
							if fd == nil {
								continue
							}
							dispatchLog(fmt.Sprintf("  -> Loaded Tool: %s", fd.Name))
						}
					}
				}

				dispatchLog(fmt.Sprintf("RAW PROMPT CHAIN - History Entries: %d", len(req.Contents)))
				for i, c := range req.Contents {
					if c == nil {
						continue
					}
					dispatchLog(fmt.Sprintf(" [Itm %d] Role: %q", i, c.Role))
					for j, p := range c.Parts {
						if p == nil {
							continue
						}
						switch {
						case p.Text != "":
							dispatchLog(fmt.Sprintf("     P%d TEXT: %q", j, p.Text))
						case p.FunctionCall != nil:
							dispatchLog(fmt.Sprintf("     P%d CALL: %s", j, p.FunctionCall.Name))
						case p.FunctionResponse != nil:
							dispatchLog(fmt.Sprintf("     P%d RESP: %s", j, p.FunctionResponse.Name))
						default:
							dispatchLog(fmt.Sprintf("     P%d OTH", j))
						}
					}
				}
				return nil, nil
			},
		},
	})

	if err != nil {
		wrappedLego.Close()
		wrappedRobot.Close()
		return nil, nil, fmt.Errorf("agent engine final build fault: %w", err)
	}

	return a, closers, nil
}
