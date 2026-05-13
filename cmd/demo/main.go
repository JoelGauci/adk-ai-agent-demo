package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	internalagent "adk-ai-agent-demo-1/internal/agent"
	"adk-ai-agent-demo-1/internal/auth"
)

//go:embed index.html
var indexHTML embed.FS

var (
	geminiAPIKey         string
	globalSessionService session.Service
)

type templateData struct {
	Authenticated bool
	UserEmail     string
	UserName      string
}

type logPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func main() {
	// Read critical configuration from environment variables.
	auth.ClientID = os.Getenv("OAUTH_CLIENT_ID")
	auth.ClientSecret = os.Getenv("OAUTH_CLIENT_SECRET")
	auth.AuthURL = os.Getenv("OAUTH_AUTHORIZE_URL")
	auth.TokenURL = os.Getenv("OAUTH_TOKEN_URL")
	auth.RedirectURI = os.Getenv("OAUTH_REDIRECT_URI")

	auth.LegoEndpoint = os.Getenv("LEGO_MCP_ENDPOINT")
	auth.RobotEndpoint = os.Getenv("ROBOT_MCP_ENDPOINT")
	geminiAPIKey = os.Getenv("GOOGLE_API_KEY")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Setup critical runner state dependency.
	globalSessionService = session.InMemoryService()

	mux := http.NewServeMux()

	tmpl := template.Must(template.ParseFS(indexHTML, "index.html"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		cookie, _ := r.Cookie("base_token")
		emailCookie, _ := r.Cookie("user_email")

		data := templateData{
			Authenticated: cookie != nil && cookie.Value != "",
			UserEmail:     "",
		}
		if emailCookie != nil {
			data.UserEmail = emailCookie.Value
		}

		if cookie != nil && cookie.Value != "" {
			protectedURL := strings.Replace(auth.TokenURL, "/token", "/protected", 1)
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, protectedURL, nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+cookie.Value)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						var protRes struct {
							Response struct {
								User struct {
									Name  string `json:"name"`
									Email string `json:"email"`
								} `json:"user"`
							} `json:"response"`
						}
						if json.NewDecoder(resp.Body).Decode(&protRes) == nil {
							if protRes.Response.User.Email != "" {
								data.UserEmail = protRes.Response.User.Email
								auth.StoreEmailForToken(cookie.Value, data.UserEmail)
							}
							data.UserName = protRes.Response.User.Name
						}
					}
				}
			}
		}

		tmpl.Execute(w, data)
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		url, err := auth.BuildAuthURL()
		if err != nil {
			http.Error(w, "Failed to build auth redirection URL", 500)
			return
		}
		http.Redirect(w, r, url, http.StatusFound)
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" {
			http.Error(w, "Missing authorization callback code", 400)
			return
		}
		tok, email, expiresIn, err := auth.ExchangeCode(code, state)
		if err != nil {
			http.Error(w, fmt.Sprintf("Authentication finalization failed: %v", err), 401)
			return
		}

		// Securely bind email profile server-side via static token lookup mapping.
		auth.StoreEmailForToken(tok, email)

		// Secure toggle: Allow local HTTP but enforce TLS safety where available.
		isSecure := strings.HasPrefix(auth.RedirectURI, "https://")

		// Propagate identical expiration boundaries to both cookies.
		http.SetCookie(w, &http.Cookie{
			Name:     "base_token",
			Value:    tok,
			Path:     "/",
			MaxAge:   expiresIn,
			Secure:   isSecure,
			HttpOnly: true,
		})

		http.SetCookie(w, &http.Cookie{
			Name:     "user_email",
			Value:    email,
			Path:     "/",
			MaxAge:   expiresIn,
			Secure:   isSecure,
			HttpOnly: true,
		})

		http.Redirect(w, r, "/", http.StatusFound)
	})

	mux.HandleFunc("/chat", handleChat)

	fmt.Printf("Google ADK Demo Application starting gracefully on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Environment does not support server streaming", 500)
		return
	}

	// Initialize SSE transmission protocol FIRST to establish listener pipe.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Validate browser cookie.
	cookie, err := r.Cookie("base_token")
	if err != nil || cookie.Value == "" {
		sendEvent(w, "log", logPayload{Type: "AUTH_REJECTED", Message: "Active session has expired. Reload required."})
		flusher.Flush()
		return
	}
	baseToken := cookie.Value

	prompt := r.URL.Query().Get("prompt")
	if prompt == "" {
		sendEvent(w, "log", logPayload{Type: "ERROR", Message: "Prompt cannot be empty."})
		flusher.Flush()
		return
	}

	// Create explicit logging pipeline to be passed into Context.
	logChan := make(chan logPayload, 100)
	logger := func(msgType string, message string) {
		select {
		case logChan <- logPayload{Type: msgType, Message: message}:
		default: // Drop if congested instead of deadlocking server threads.
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ctx = context.WithValue(ctx, auth.TransportLoggerKey{}, auth.LogFunc(logger))

	// Initialize dynamic localized agent ecosystem.
	agentInstance, closers, err := internalagent.BuildUserAgent(ctx, geminiAPIKey, baseToken)
	defer func() {
		for _, c := range closers {
			c.Close() // Hard cleanup to release background routines/conns.
		}
	}()

	if err != nil {
		sendEvent(w, "log", logPayload{Type: "ERROR", Message: fmt.Sprintf("Construction failed: %v", err)})
		flusher.Flush()
		return
	}

	run, err := runner.New(runner.Config{
		Agent:             agentInstance,
		SessionService:    globalSessionService,
		AppName:           "adk-ai-demo",
		AutoCreateSession: true,
	})
	if err != nil {
		sendEvent(w, "log", logPayload{Type: "ERROR", Message: fmt.Sprintf("Runner crash: %v", err)})
		flusher.Flush()
		return
	}

	textChunkChan := make(chan string, 10)
	errSignalChan := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(1)

	targetUserID, identityFound := auth.GetEmailForToken(baseToken)
	if !identityFound || targetUserID == "" {
		sendEvent(w, "log", logPayload{Type: "AUTH_REJECTED", Message: "Identity verification failed. Please relogin."})
		flusher.Flush()
		return
	}

	hasher := sha256.New()
	hasher.Write([]byte(targetUserID))
	sessionID := fmt.Sprintf("%x", hasher.Sum(nil))

	content := &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: prompt}},
	}

	go func() {
		defer wg.Done()
		defer close(textChunkChan)

		prevText := ""

		iter := run.Run(ctx, targetUserID, sessionID, content, agent.RunConfig{StreamingMode: agent.StreamingModeSSE})

		for event, iterErr := range iter {
			if iterErr != nil {
				errSignalChan <- iterErr
				return
			}
			if event == nil || event.Content == nil {
				continue
			}

			var currentText string
			for _, p := range event.Content.Parts {
				if p.Text != "" {
					currentText += p.Text
				}
			}

			if currentText == "" {
				continue
			}

			if event.LLMResponse.Partial {
				prevText += currentText
				select {
				case <-ctx.Done():
					return
				case textChunkChan <- currentText:
				}
				continue
			}

			if currentText != prevText {
				select {
				case <-ctx.Done():
					return
				case textChunkChan <- currentText:
				}
			}

			prevText = ""
		}
	}()

Loop:
	for {
		select {
		case <-ctx.Done():
			break Loop
		case logItem := <-logChan:
			sendEvent(w, "log", logItem)
			flusher.Flush()
		case chunk, more := <-textChunkChan:
			if !more {
				break Loop
			}
			sendEvent(w, "chunk", chunk)
			flusher.Flush()
		case runtimeErr := <-errSignalChan:
			sendEvent(w, "log", logPayload{Type: "ERROR", Message: fmt.Sprintf("Runtime execution defect: %v", runtimeErr)})
			flusher.Flush()
			break Loop
		}
	}

	sendEvent(w, "done", "{}")
	flusher.Flush()

	cancel()
	wg.Wait()
}

func sendEvent(w http.ResponseWriter, eventName string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(b))
}
