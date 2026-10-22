package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runMCPStdio is "wiser-dashboard mcp": it relays MCP over stdio, which is how
// desktop apps start local MCP servers, to a dashboard's /mcp over HTTP.
// stdout carries only MCP messages; problems go to stderr.
func runMCPStdio(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	target := fs.String("url", envOr("WISER_MCP_URL", "http://localhost:8080/mcp"), "the dashboard's MCP address (or WISER_MCP_URL)")
	token := fs.String("token", os.Getenv("WISER_MCP_TOKEN"), "the MCP token from Advanced → AI assistants (or WISER_MCP_TOKEN)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wiser-dashboard mcp -url http://<dashboard>:8080/mcp -token <token>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *token == "" {
		fmt.Fprintln(os.Stderr, "wiser-dashboard mcp: no token; pass -token or set WISER_MCP_TOKEN")
		os.Exit(2)
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	out := bufio.NewWriter(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		reply, err := relay(client, *target, *token, line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wiser-dashboard mcp:", err)
			reply = errorReply(line, err)
		}
		if len(reply) > 0 {
			out.Write(reply)
			out.WriteByte('\n')
			out.Flush()
		}
	}
}

// relay posts one message and returns the reply, compacted to one line.
func relay(c *http.Client, target, token string, msg []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", target, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach the dashboard at %s: %v", target, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	switch {
	case res.StatusCode == http.StatusAccepted:
		return nil, nil
	case res.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("the dashboard refused the MCP token (copy it again from Advanced → AI assistants)")
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the dashboard answered HTTP %d: %s", res.StatusCode, bytes.TrimSpace(body))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, fmt.Errorf("unexpected reply from the dashboard: %v", err)
	}
	return compact.Bytes(), nil
}

// errorReply answers a request the relay couldn't deliver, so the app shows
// the reason instead of waiting; notifications get no reply.
func errorReply(msg []byte, err error) []byte {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(msg, &req) != nil || len(req.ID) == 0 {
		return nil
	}
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32000, err.Error()}})
	return b
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
