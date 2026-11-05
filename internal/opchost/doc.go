// Package opchost is the host-side half of the "bridge type" plugin: a shared
// magpie-bridge.exe that implements the AIWorkGate plugin contract on top of an
// existing magpie .mjs plugin.
//
// # Who manages whom
//
// The host is the only manager. internal/plugins (Manager + supervisor)
// installs, discovers, launches, health-checks, restarts, assigns the port and
// the token, injects the environment, owns egress and captures the log of every
// plugin process. A bridge is an ordinary plugin child: it never talks to the
// supervisor, it only serves the contract HTTP API on the loopback port the
// supervisor handed it. Nothing in this package starts, stops or supervises a
// process; it produces the manifest that lets the existing supervisor launch the
// shared bridge unchanged (plan 2026-11-05 §P1 item 2: entry.exe points at the
// shared bridge, entry.args point at the plugin directory, supervisor untouched).
//
// # Who allocates the port and the token
//
// The host, and this package never re-implements it:
//
//   - the port comes from the supervisor's allocatePort
//     (internal/plugins/supervisor.go:147, :479-494): reserve a free loopback
//     port by listening on 127.0.0.1:0 and closing the listener immediately, so
//     the child — not the host — owns the listener;
//   - the token comes from tokenFor -> secrets.Store.EnsurePluginToken
//     (internal/plugins/supervisor.go:463-477, internal/secrets/secrets.go:151-175):
//     32 random bytes, hex encoded and persisted per plugin;
//   - both are injected as environment variables on every launch
//     (internal/plugins/env.go:26-33, :62-71): AIWORKGATE_PLUGIN_ID,
//     AIWORKGATE_PLUGIN_PORT, AIWORKGATE_PLUGIN_TOKEN,
//     AIWORKGATE_PLUGIN_DATA_DIR, AIWORKGATE_RULES_FILE, AIWORKGATE_EGRESS_FILE,
//     AIWORKGATE_API_LEVEL. The supervisor passes entry.args verbatim and sets
//     cmd.Dir to the plugin directory (internal/plugins/supervisor.go:166-168).
//
// manifest.Entry has no environment field (internal/model/model.go:170-173), so
// entry.args cannot carry a port or a token that the supervisor would honour.
// That is why GenerateBridgeManifest writes only Exe and ["--plugin-dir", dir]:
// the bridge takes everything else from the environment, which stays correct
// across restarts (a restart re-allocates the port). A bridge must prefer the
// environment over any argument, or it will bind a port the supervisor is not
// checking.
//
// StandaloneCommand exists for hand-run/debug launches (the P1 acceptance needs
// to drive the bridge directly before it is wired into the installer). It asks an
// injected Allocator for the port and token, whose production implementations are
// exactly allocatePort and EnsurePluginToken above.
//
// # How the "shared" executable reaches a plugin directory
//
// The plugin contract only accepts a relative entry.exe that stays inside the
// plugin directory (internal/plugins/manifest.go validateEntryPath rejects ".."
// escapes and drive-qualified paths), and the supervisor resolves it as
// filepath.Join(pluginDir, entry.exe). A shared copy therefore cannot be
// referenced by path from inside every plugin directory. The supported layout
// keeps one physical binary at, for example, <dataDir>\bridge\magpie-bridge.exe
// and deploys it into each plugin directory as a hard link (same file, one copy
// on disk) or a copy named magpie-bridge.exe; the manifest then names that
// in-directory entry. This package computes and validates that entry path; it
// does not write files, because deployment stays with the installer that already
// owns the plugin directory (<dataDir>\plugins\<id>, with credentials under
// <dataDir>\plugin-data\<id>; internal/plugins/manager.go:205-227).
//
// # Streaming
//
// Chat streaming is a copy-through in the gateway, not a parser: 32 KiB chunks,
// flushed per chunk, with ": keep-alive\n\n" written when the upstream is silent
// for 15 s (internal/gateway/gateway.go:355-467, :1327). Upstream headers are
// bounded by ResponseHeaderTimeout = 90 s (:151) and a stalled body by a 5 minute
// idle read (:152), but there is no overall client timeout for a stream. A bridge
// must not add its own stream deadline: it forwards bytes and lets the gateway's
// context cancel the request.
//
// # Keep in sync
//
// The paths, timeouts and structs here mirror internal/plugins/client.go and
// internal/plugins/supervisor.go. They are copied rather than imported so this
// package depends on internal/model only: importing internal/plugins would make
// a future plugins -> opchost manifest-generation path a cycle.
package opchost
