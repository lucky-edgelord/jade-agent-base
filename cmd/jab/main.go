// jab: the Jade Agent Base server. PocketBase (auth, collections, realtime, files, admin UI at /_/) plus modules:
//
//	jab serve --modules agent            # the default
//	jab agent create shopkeeper --scopes read,write,schema,hooks,site
//
// Every PocketBase command and flag works too (superuser, migrate, …).
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lucky-edgelord/jade-agent-base/agent"
	"github.com/lucky-edgelord/jade-agent-base/jab"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/osutils"
)

func main() {
	app := pocketbase.New()
	app.RootCmd.Use = "jab"
	app.RootCmd.Short = "Jade Agent Base: a backend AI agents can run"
	app.RootCmd.Version = jab.Version + " (PocketBase " + pocketbase.Version + ")"

	var modules, hooksDir, migrationsDir, publicDir string
	var hooksWatch, automigrate, indexFallback bool
	var hooksPool int
	f := app.RootCmd.PersistentFlags()
	f.StringVar(&modules, "modules", env("JAB_MODULES", ""), "modules to switch on, comma-separated, or all (default agent; env JAB_MODULES)")
	f.StringVar(&hooksDir, "hooksDir", "", "the directory with the JS app hooks (default pb_hooks next to pb_data)")
	f.BoolVar(&hooksWatch, "hooksWatch", true, "reload when pb_hooks change (agents write hooks)")
	f.IntVar(&hooksPool, "hooksPool", 15, "prewarmed JS runtimes for hooks")
	f.StringVar(&migrationsDir, "migrationsDir", "", "the directory with user migrations")
	f.BoolVar(&automigrate, "automigrate", true, "make migration files when collections change")
	f.StringVar(&publicDir, "publicDir", defaultPublicDir(), "the directory served at / (agents write the site here)")
	f.BoolVar(&indexFallback, "indexFallback", true, "serve index.html for unknown paths (single-page apps)")
	_ = app.RootCmd.ParseFlags(os.Args[1:])

	mods, err := jab.ParseModules(modules)
	if err != nil {
		log.Fatal(err)
	}
	if hooksDir == "" {
		hooksDir = filepath.Join(app.DataDir(), "..", "pb_hooks")
	}
	// the hooks watcher only watches a folder that exists, and agents write the first hook later
	_ = os.MkdirAll(hooksDir, 0o755)
	jsvm.MustRegister(app, jsvm.Config{MigrationsDir: migrationsDir, HooksDir: hooksDir, HooksWatch: hooksWatch, HooksPoolSize: hooksPool})
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{TemplateLang: migratecmd.TemplateLangJS, Automigrate: automigrate, Dir: migrationsDir})
	if err := jab.Setup(app, jab.Options{Modules: mods, HooksDir: hooksDir, PublicDir: publicDir}); err != nil {
		log.Fatal(err)
	}
	for _, m := range mods {
		if m == "agent" {
			app.RootCmd.AddCommand(agent.Command(app))
		}
	}
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Func: func(e *core.ServeEvent) error {
			if !e.Router.HasRoute(http.MethodGet, "/{path...}") {
				e.Router.GET("/{path...}", apis.Static(os.DirFS(publicDir), indexFallback))
			}
			return e.Next()
		},
		Priority: 999,
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func defaultPublicDir() string {
	if osutils.IsProbablyGoRun() {
		return "./pb_public"
	}
	return filepath.Join(os.Args[0], "../pb_public")
}
