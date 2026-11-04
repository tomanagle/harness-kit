// Wails example: the shared Harness Kit dashboard (../ui) in a Wails app.
//
// By default the backend runs the harness-kit sidecar and relays raw
// protocol lines to the frontend, which uses the same @tomanagle/harness-kit
// as the Electron example (see bridge.go); the demo tools are registered
// in the webview. Set HARNESS_KIT_MODE=direct to link Harness Kit
// in-process instead, with typed Wails bindings and Go demo tools (see
// direct.go): no second process, no RPC hop.
//
//	make example wails                          # sidecar mode
//	HARNESS_KIT_MODE=direct make example wails  # direct mode
package main

import (
	"context"
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/tomanagle/harness-kit/core/toolhost"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// In direct mode this binary is also the MCP bridge that runtimes launch
	// to reach the Go host tools: relay and exit when started as one.
	toolhost.ServeBridgeIfRequested()
	var bind []any
	var onStartup, onShutdown func(ctx context.Context)
	if os.Getenv("HARNESS_KIT_MODE") == "direct" {
		d := NewDirect()
		bind, onStartup, onShutdown = []any{d}, d.startup, d.shutdown
	} else {
		b := NewBridge(sidecarPath())
		bind, onStartup, onShutdown = []any{b}, b.startup, b.shutdown
	}
	err := wails.Run(&options.App{
		Title:       "Harness Kit · Wails",
		Width:       900,
		Height:      700,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   onStartup,
		OnShutdown:  onShutdown,
		Bind:        bind,
	})
	if err != nil {
		log.Fatal(err)
	}
}
