// Command aiworkgate runs the AIWorkGate Windows host.
package main

import (
	"errors"
	"fmt"
	"os"

	"aiworkgate/internal/app"
)

func main() {
	host, err := app.New()
	if err != nil {
		if errors.Is(err, app.ErrAlreadyRunning) {
			// A previous instance owns the mutex and its web UI was opened.
			return
		}
		fmt.Fprintf(os.Stderr, "aiworkgate: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = host.Shutdown() }()

	if err := host.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiworkgate: %v\n", err)
		os.Exit(1)
	}
}
