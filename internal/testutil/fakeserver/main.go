package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if os.Getenv("FAKE_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "fake server: FAKE_FAIL set")
		os.Exit(1)
	}
	appendLine(os.Getenv("FAKE_SPAWN_LOG"), strconv.Itoa(os.Getpid()))
	if os.Getenv("FAKE_GRANDCHILD") == "1" {
		spawnGrandchild()
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "fakeserver", Version: "0.0.1"}, &mcp.ServerOptions{
		Instructions:       "fake upstream instructions",
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
		CompletionHandler: func(_ context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			var values []string
			if req.Params.Ref != nil && req.Params.Ref.Type == "ref/prompt" && req.Params.Ref.Name == "greet" {
				for _, n := range []string{"alice", "alex", "bob"} {
					if strings.HasPrefix(n, req.Params.Argument.Value) {
						values = append(values, n)
					}
				}
			}
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: values, Total: len(values)}}, nil
		},
	})
	addTools(server)
	addContent(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("fake server: %v", err)
	}
}

func addTools(server *mcp.Server) {
	type echoArgs struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text"},
		func(_ context.Context, _ *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
			return textResult(a.Text), nil, nil
		})

	type slowArgs struct {
		Millis int `json:"millis"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "slow", Description: "sleep then return"},
		func(ctx context.Context, _ *mcp.CallToolRequest, a slowArgs) (*mcp.CallToolResult, any, error) {
			select {
			case <-time.After(time.Duration(a.Millis) * time.Millisecond):
				return textResult("done"), nil, nil
			case <-ctx.Done():
				appendLine(os.Getenv("FAKE_CANCEL_LOG"), "slow cancelled")
				return nil, nil, ctx.Err()
			}
		})

	mcp.AddTool(server, &mcp.Tool{Name: "progress", Description: "send three progress notifications"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			token := req.Params.GetProgressToken()
			if token == nil {
				return textResult("no progress token"), nil, nil
			}
			for i := 1; i <= 3; i++ {
				err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token,
					Progress:      float64(i),
					Total:         3,
					Message:       fmt.Sprintf("step %d", i),
				})
				if err != nil {
					return nil, nil, err
				}
			}
			return textResult("progressed"), nil, nil
		})

	type touchArgs struct {
		URI string `json:"uri"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "touch", Description: "notify that a resource was updated"},
		func(ctx context.Context, _ *mcp.CallToolRequest, a touchArgs) (*mcp.CallToolResult, any, error) {
			if err := server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: a.URI}); err != nil {
				return nil, nil, err
			}
			return textResult("touched " + a.URI), nil, nil
		})

	type addArgs struct {
		Name string `json:"name"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "add_tool", Description: "register a new echo-like tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, a addArgs) (*mcp.CallToolResult, any, error) {
			name := a.Name
			mcp.AddTool(server, &mcp.Tool{Name: name, Description: "dynamic tool"},
				func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
					return textResult("dynamic " + name), nil, nil
				})
			return textResult("added " + name), nil, nil
		})
}

func addContent(server *mcp.Server) {
	server.AddResource(&mcp.Resource{URI: "fake://a", Name: "a", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "resource a"}}}, nil
		})

	server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "fake://item/{id}", Name: "item", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			id := strings.TrimPrefix(req.Params.URI, "fake://item/")
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "item " + id}}}, nil
		})

	server.AddPrompt(&mcp.Prompt{
		Name:        "greet",
		Description: "greet someone",
		Arguments:   []*mcp.PromptArgument{{Name: "name", Required: true}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["name"]}}},
		}, nil
	})
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func spawnGrandchild() {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		log.Fatalf("spawn grandchild: %v", err)
	}
	appendLine(os.Getenv("FAKE_GRANDCHILD_LOG"), strconv.Itoa(cmd.Process.Pid))
}

func appendLine(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}
