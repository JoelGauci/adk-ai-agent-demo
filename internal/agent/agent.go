package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
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

type DisplayAllStepsArgs struct {
	ModelId      string `json:"modelId"`
	DelaySeconds int    `json:"delaySeconds"`
}

type DisplayAllStepsResult struct {
	Status         string `json:"status"`
	ModelId        string `json:"modelId"`
	StepsDisplayed int    `json:"stepsDisplayed"`
	DelaySeconds   int    `json:"delaySeconds"`
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

	var getModelTool tool.Tool
	var activateTool tool.Tool
	var mutateTool tool.Tool
	var startupTools []tool.Tool
	var initOnce sync.Once

	displayAllStepsTool, err := functiontool.New(functiontool.Config{
		Name:        "display_all_model_steps_with_delay",
		Description: "Display all construction/building steps of a Lego model sequentially with a timed delay between each step. By default, the delay is 2 seconds, starting from step 0 up to the final step.",
	}, func(tCtx tool.Context, args DisplayAllStepsArgs) (DisplayAllStepsResult, error) {
		delay := args.DelaySeconds
		if delay <= 0 {
			delay = 2
		}

		var logger auth.LogFunc
		if l, ok := tCtx.Value(auth.TransportLoggerKey{}).(auth.LogFunc); ok {
			logger = l
		}

		// Retrieve tools once using the proper valid runtime tool.Context
		initOnce.Do(func() {
			startupTools, _ = legoTS.Tools(tCtx)
			for _, t := range startupTools {
				desc := strings.ToLower(t.Description())
				name := strings.ToLower(t.Name())

				if mcpT, ok := t.(interface{ Declaration() *genai.FunctionDeclaration }); ok && mcpT != nil {
					decl := mcpT.Declaration()
					b, _ := json.Marshal(decl)
					if logger != nil {
						logger("AUDIT", fmt.Sprintf("Loaded Lego Tool Schema: %s", string(b)))
					}
				}

				if strings.Contains(desc, "detail metrics") || strings.Contains(name, "get_models_id") || (strings.Contains(name, "models") && !strings.Contains(name, "display") && !strings.Contains(name, "file") && !strings.Contains(name, "catalog")) {
					getModelTool = t
				}
				// Specifically capture the POST activation tool to mount the model
				if strings.Contains(desc, "activate model rendering") || strings.Contains(name, "post_models_id_display") {
					activateTool = t
				}
				// Specifically capture the PATCH mutation tool to set individual building steps
				if strings.Contains(desc, "mutate active view") || strings.Contains(name, "patch_models_id_display") {
					mutateTool = t
				}
			}

			// Fallbacks if specific tools weren't cleanly matched by description
			if activateTool == nil {
				for _, t := range startupTools {
					if strings.Contains(strings.ToLower(t.Name()), "display") {
						activateTool = t
						break
					}
				}
			}
			if mutateTool == nil {
				mutateTool = activateTool
			}
		})

		if getModelTool == nil || activateTool == nil {
			var available []string
			for _, t := range startupTools {
				available = append(available, t.Name())
			}
			return DisplayAllStepsResult{}, fmt.Errorf("required MCP tools not found in legoTS. Available tools: %v", available)
		}

		callTool := func(targetTool tool.Tool, toolArgs map[string]any) (map[string]any, error) {
			v := reflect.ValueOf(targetTool)
			m := v.MethodByName("Run")
			if !m.IsValid() {
				return nil, fmt.Errorf("Run method not found on tool %s", targetTool.Name())
			}
			res := m.Call([]reflect.Value{reflect.ValueOf(tCtx), reflect.ValueOf(toolArgs)})

			errVal := res[1].Interface()
			if errVal != nil {
				return nil, errVal.(error)
			}

			outVal := res[0].Interface()
			if outVal == nil {
				return nil, nil
			}
			return outVal.(map[string]any), nil
		}

		// 1. Get model details to find NumBuildingSteps
		out, err := callTool(getModelTool, map[string]any{"id": args.ModelId})
		if err != nil {
			return DisplayAllStepsResult{}, fmt.Errorf("failed calling %s: %w", getModelTool.Name(), err)
		}

		var numSteps int
		if out != nil && out["output"] != nil {
			switch val := out["output"].(type) {
			case map[string]any:
				if v, ok := val["numBuildingSteps"].(float64); ok {
					numSteps = int(v)
				}
			case string:
				var meta struct {
					NumBuildingSteps int `json:"numBuildingSteps"`
				}
				if json.Unmarshal([]byte(val), &meta) == nil {
					numSteps = meta.NumBuildingSteps
				}
			}
		}

		if numSteps <= 0 {
			numSteps = 25
		}

		// 2. Ensure model is actively mounted on viewport by POSTing step 0 first
		if logger != nil {
			logger("AUDIT", fmt.Sprintf("Mounting model %s at step 0 via %s", args.ModelId, activateTool.Name()))
		}
		_, _ = callTool(activateTool, map[string]any{
			"id":           args.ModelId,
			"buildingStep": 0,
			"ViewerState":  map[string]any{"buildingStep": 0},
			"requestBody":  map[string]any{"buildingStep": 0},
		})
		time.Sleep(300 * time.Millisecond)

		// 3. Loop through steps 0 to N-1 using the PATCH mutation tool with both flat and nested schema bindings
		for i := 0; i < numSteps; i++ {
			if logger != nil {
				logger("AUDIT", fmt.Sprintf("Affichage de l'étape %d/%d pour le modèle %s via l'outil %s", i, numSteps-1, args.ModelId, mutateTool.Name()))
			}

			_, err := callTool(mutateTool, map[string]any{
				"id":           args.ModelId,
				"buildingStep": i,
				"ViewerState":  map[string]any{"buildingStep": i},
				"requestBody":  map[string]any{"buildingStep": i},
			})
			if err != nil {
				return DisplayAllStepsResult{}, fmt.Errorf("failed setting building step %d via %s: %w", i, mutateTool.Name(), err)
			}

			if i < numSteps-1 {
				select {
				case <-tCtx.Done():
					return DisplayAllStepsResult{}, tCtx.Err()
				case <-time.After(time.Duration(delay) * time.Second):
				}
			}
		}

		return DisplayAllStepsResult{
			Status:         "success",
			ModelId:        args.ModelId,
			StepsDisplayed: numSteps,
			DelaySeconds:   delay,
		}, nil
	})
	if err != nil {
		wrappedLego.Close()
		wrappedRobot.Close()
		return nil, nil, fmt.Errorf("displayAllStepsTool creation crash: %w", err)
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
			- TEMPORIZATION: To display all manufacturing/building steps of a model sequentially with a delay/temporization, you MUST use the tool display_all_model_steps_with_delay.
			- FORMATTING: ALWAYS present output as narrative summaries using clean paragraphs. NEVER display raw JSON schemas.
			- LANGUAGE MANDATE: You MUST strictly reply in the exact same language as the user's most recent prompt. If the user asks in English, reply in English. If the user asks in French, reply in French.
			</directives>
		`,
		Model:    geminiModel,
		Tools:    []tool.Tool{displayAllStepsTool},
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
