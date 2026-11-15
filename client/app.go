package main

import (
	"context"

	"ircapybara/client/internal/core"
)

// App is the object bound into the Wails webview. The embedded core supplies
// the whole frontend API, so its methods appear as window.go.main.App.* without
// per-method wrappers here.
type App struct {
	*core.App
	host *desktopHost
}

func NewApp() *App {
	host := &desktopHost{}
	return &App{
		App:  core.New(core.Options{Host: host, Platform: core.PlatformDesktop}),
		host: host,
	}
}

func (a *App) startup(ctx context.Context) {
	a.host.start(ctx)
}

func (a *App) shutdown(context.Context) {
	a.App.Shutdown()
	a.host.stop()
}
