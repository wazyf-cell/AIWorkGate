// Command devsession prints the running host's admin launch token or a usable
// session cookie. It exists so local verification scripts can call the admin
// API the same way the browser does, without weakening the web UI auth gate.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"aiworkgate/internal/secrets"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: devsession token|cookie [base-url]")
		os.Exit(2)
	}
	dataDir := filepath.Join(os.Getenv("APPDATA"), "AIWorkGate")
	store, err := secrets.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devsession: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	token, ok := store.GetExtra("admin_launch_token")
	if !ok || token == "" {
		fmt.Fprintln(os.Stderr, "devsession: no launch token stored")
		os.Exit(1)
	}
	if os.Args[1] == "token" {
		fmt.Print(token)
		return
	}
	base := "http://127.0.0.1:7865"
	if len(os.Args) > 2 {
		base = os.Args[2]
	}
	resp, err := http.Post(base+"/api/session?t="+token, "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Fprintf(os.Stderr, "devsession: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "devsession: session = %d %s\n", resp.StatusCode, body)
		os.Exit(1)
	}
	for _, c := range resp.Cookies() {
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		fmt.Printf("%s=%s\n", c.Name, c.Value)
		return
	}
	fmt.Fprintln(os.Stderr, "devsession: no session cookie in response:", string(body))
	os.Exit(1)
}
