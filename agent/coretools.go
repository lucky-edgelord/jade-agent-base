package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dop251/goja"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

const maxFile = 10 << 20

func coreTools() {
	AddTool(&Tool{Name: "server_info", Scope: ScopeRead,
		Description: "What this server is: version, modules, your scopes, collections, and where hooks and the site live. Start here.",
		Run: func(c *Call) (any, error) {
			cols, _ := c.App.FindAllCollections()
			names := []string{}
			for _, col := range cols {
				if !col.System {
					names = append(names, col.Name)
				}
			}
			sc := []string{}
			for _, s := range AllScopes {
				if c.Scopes[s] {
					sc = append(sc, s)
				}
			}
			who := "superuser"
			if c.Actor != nil && !c.Actor.IsSuperuser() {
				who = "agent " + c.Actor.GetString("name")
			}
			return map[string]any{"version": cfg.Version, "modules": cfg.Modules, "you": who, "scopes": sc, "collections": names,
				"appName": c.App.Settings().Meta.AppName, "appURL": c.App.Settings().Meta.AppURL, "go": runtime.Version(),
				"hooks":  "server-side JavaScript in pb_hooks (*.pb.js), reloaded on change: routes, record hooks, cron jobs",
				"site":   "static files in pb_public, served at /",
				"notes":  "Collections and records follow PocketBase: rules are filter strings (\"\" = anyone, null = superusers only). Records API for apps: /api/collections/{name}/records."}, nil
		}})

	// ---- collections ----
	AddTool(&Tool{Name: "collections_list", Scope: ScopeRead,
		Description: "List collections with their type and fields (name:type, * = required). System collections are left out unless system is true.",
		Parameters:  obj(map[string]any{"system": boo("include system collections")}),
		Run: func(c *Call) (any, error) {
			cols, err := c.App.FindAllCollections()
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, col := range cols {
				if col.System && !c.Bool("system") {
					continue
				}
				fields := []string{}
				for _, f := range col.Fields {
					if f.GetHidden() && f.GetSystem() {
						continue
					}
					s := f.GetName() + ":" + f.Type()
					if req, ok := fieldRequired(f); ok && req {
						s += "*"
					}
					fields = append(fields, s)
				}
				out = append(out, map[string]any{"name": col.Name, "type": col.Type, "fields": fields})
			}
			return out, nil
		}})
	AddTool(&Tool{Name: "collection_get", Scope: ScopeRead,
		Description: "A collection's full definition: fields with options, API rules, indexes.",
		Parameters:  obj(map[string]any{"name": str("collection name or id")}, "name"),
		Run: func(c *Call) (any, error) { return c.App.FindCollectionByNameOrId(c.Str("name")) }})
	AddTool(&Tool{Name: "collection_create", Scope: ScopeSchema,
		Description: `Create a collection. type: base (default), auth (users that sign in) or view (a read-only SQL query in viewQuery).
fields: [{"name":"title","type":"text","required":true}, {"name":"price","type":"number"}, {"name":"owner","type":"relation","collectionId":"<id or name>","maxSelect":1}, {"name":"tags","type":"select","values":["a","b"],"maxSelect":3}, {"name":"photo","type":"file","maxSelect":1}, {"name":"created","type":"autodate","onCreate":true}]
Field types: text, number, bool, email, url, date, autodate, select, file, relation, json, editor, geoPoint, password.
Rules are PocketBase filters: "" lets anyone, null (or leaving it out) only superusers, e.g. "@request.auth.id != ''" or "owner = @request.auth.id".`,
		Parameters: obj(map[string]any{"name": str("collection name (letters, digits, _)"), "type": str("base | auth | view"),
			"fields": arr("fields", map[string]any{"type": "object"}), "indexes": arr("CREATE INDEX statements", map[string]any{"type": "string"}),
			"listRule": str("who may list"), "viewRule": str("who may view"), "createRule": str("who may create"), "updateRule": str("who may update"), "deleteRule": str("who may delete"),
			"viewQuery": str("SQL for a view collection")}, "name"),
		Run: func(c *Call) (any, error) {
			typ := c.Str("type")
			var col *core.Collection
			switch typ {
			case "", "base":
				col = core.NewBaseCollection(c.Str("name"))
			case "auth":
				col = core.NewAuthCollection(c.Str("name"))
			case "view":
				col = core.NewViewCollection(c.Str("name"))
			default:
				return nil, fmt.Errorf("type must be base, auth or view")
			}
			patch := map[string]any{}
			for k, v := range c.Args {
				if k != "type" && k != "name" && k != "fields" {
					patch[k] = v
				}
			}
			if err := applyJSON(col, patch); err != nil {
				return nil, err
			}
			if err := addFields(c.App, col, c.List("fields")); err != nil {
				return nil, err
			}
			if err := c.App.Save(col); err != nil {
				return nil, explain(err)
			}
			return col, nil
		}})
	AddTool(&Tool{Name: "collection_update", Scope: ScopeSchema,
		Description: "Change a collection: set any of its properties (rename with newName, rules, indexes), add fields, change fields by name, remove fields. Removing a field deletes its data.",
		Parameters: obj(map[string]any{"name": str("collection name or id"), "newName": str("rename"),
			"set":          objp(`properties to set, e.g. {"listRule": "", "indexes": [...]}`),
			"addFields":    arr("fields to add (same shape as in collection_create)", map[string]any{"type": "object"}),
			"updateFields": arr(`fields to change, by name: [{"name":"title","max":200}]`, map[string]any{"type": "object"}),
			"removeFields": arr("field names to remove", map[string]any{"type": "string"})}, "name"),
		Destructive: true,
		Run: func(c *Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId(c.Str("name"))
			if err != nil {
				return nil, fmt.Errorf("no collection %q", c.Str("name"))
			}
			if set := c.Map("set"); set != nil {
				delete(set, "fields")
				delete(set, "id")
				if err := applyJSON(col, set); err != nil {
					return nil, err
				}
			}
			if n := c.Str("newName"); n != "" {
				col.Name = n
			}
			for _, x := range c.List("removeFields") {
				n, _ := x.(string)
				if col.Fields.GetByName(n) == nil {
					return nil, fmt.Errorf("no field %q", n)
				}
				col.Fields.RemoveByName(n)
			}
			for _, x := range c.List("updateFields") {
				m, _ := x.(map[string]any)
				n, _ := m["name"].(string)
				f := col.Fields.GetByName(n)
				if f == nil {
					return nil, fmt.Errorf("no field %q to update", n)
				}
				if err := applyJSON(f, m); err != nil {
					return nil, err
				}
			}
			if err := addFields(c.App, col, c.List("addFields")); err != nil {
				return nil, err
			}
			if err := c.App.Save(col); err != nil {
				return nil, explain(err)
			}
			return col, nil
		}})
	AddTool(&Tool{Name: "collection_delete", Scope: ScopeSchema, Destructive: true,
		Description: "Delete a collection and all its records. Pass confirm: the collection's name.",
		Parameters:  obj(map[string]any{"name": str("collection name or id"), "confirm": str("the name again")}, "name", "confirm"),
		Run: func(c *Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId(c.Str("name"))
			if err != nil {
				return nil, fmt.Errorf("no collection %q", c.Str("name"))
			}
			if c.Str("confirm") != col.Name {
				return nil, fmt.Errorf("confirm must be %q", col.Name)
			}
			if col.System || col.Name == AgentsCollection || col.Name == AuditCollection {
				return nil, fmt.Errorf("%q cannot be deleted by an agent", col.Name)
			}
			if err := c.App.Delete(col); err != nil {
				return nil, explain(err)
			}
			return "deleted " + col.Name, nil
		}})

	// ---- records ----
	AddTool(&Tool{Name: "records_list", Scope: ScopeRead,
		Description: `List records. filter uses PocketBase syntax: "status = 'open' && total > 10", "title ~ 'shoe'", "created >= '2026-01-01'". sort: "-created,title". As an agent you see every record (API rules do not apply).`,
		Parameters: obj(map[string]any{"collection": str("name or id"), "filter": str("filter expression"), "sort": str("e.g. -created"),
			"limit": num("default 30, at most 500"), "offset": num("skip this many"), "expand": str("relation fields to include, e.g. owner,tags")}, "collection"),
		Run: func(c *Call) (any, error) {
			limit := c.Int("limit", 30)
			if limit < 1 || limit > 500 {
				limit = 500
			}
			rs, err := c.App.FindRecordsByFilter(c.Str("collection"), c.Str("filter"), c.Str("sort"), limit+1, c.Int("offset", 0))
			if err != nil {
				return nil, explain(err)
			}
			more := len(rs) > limit
			if more {
				rs = rs[:limit]
			}
			if ex := c.Str("expand"); ex != "" {
				if errs := c.App.ExpandRecords(rs, strings.Split(ex, ","), nil); len(errs) > 0 {
					for k, e := range errs {
						return nil, fmt.Errorf("expand %s: %w", k, e)
					}
				}
			}
			return map[string]any{"items": export(rs), "count": len(rs), "more": more}, nil
		}})
	AddTool(&Tool{Name: "record_get", Scope: ScopeRead, Description: "One record by id.",
		Parameters: obj(map[string]any{"collection": str("name or id"), "id": str("record id"), "expand": str("relation fields to include")}, "collection", "id"),
		Run: func(c *Call) (any, error) {
			r, err := c.App.FindRecordById(c.Str("collection"), c.Str("id"))
			if err != nil {
				return nil, fmt.Errorf("no record %q in %q", c.Str("id"), c.Str("collection"))
			}
			if ex := c.Str("expand"); ex != "" {
				c.App.ExpandRecord(r, strings.Split(ex, ","), nil)
			}
			return export([]*core.Record{r})[0], nil
		}})
	AddTool(&Tool{Name: "record_create", Scope: ScopeWrite,
		Description: "Create a record. data: field values (for auth collections include email and password).",
		Parameters:  obj(map[string]any{"collection": str("name or id"), "data": objp("field values")}, "collection", "data"),
		Run: func(c *Call) (any, error) {
			col, err := c.App.FindCollectionByNameOrId(c.Str("collection"))
			if err != nil {
				return nil, fmt.Errorf("no collection %q", c.Str("collection"))
			}
			r := core.NewRecord(col)
			setAll(r, c.Map("data"))
			if err := c.App.Save(r); err != nil {
				return nil, explain(err)
			}
			return export([]*core.Record{r})[0], nil
		}})
	AddTool(&Tool{Name: "record_update", Scope: ScopeWrite,
		Description: `Change fields of a record. For select/relation/file lists, "field+" appends and "field-" removes, e.g. {"tags+": "new"}.`,
		Parameters:  obj(map[string]any{"collection": str("name or id"), "id": str("record id"), "data": objp("fields to change")}, "collection", "id", "data"),
		Run: func(c *Call) (any, error) {
			r, err := c.App.FindRecordById(c.Str("collection"), c.Str("id"))
			if err != nil {
				return nil, fmt.Errorf("no record %q in %q", c.Str("id"), c.Str("collection"))
			}
			setAll(r, c.Map("data"))
			if err := c.App.Save(r); err != nil {
				return nil, explain(err)
			}
			return export([]*core.Record{r})[0], nil
		}})
	AddTool(&Tool{Name: "record_delete", Scope: ScopeWrite, Destructive: true, Description: "Delete a record.",
		Parameters: obj(map[string]any{"collection": str("name or id"), "id": str("record id")}, "collection", "id"),
		Run: func(c *Call) (any, error) {
			r, err := c.App.FindRecordById(c.Str("collection"), c.Str("id"))
			if err != nil {
				return nil, fmt.Errorf("no record %q in %q", c.Str("id"), c.Str("collection"))
			}
			if r.Collection().Name == AuditCollection {
				return nil, errors.New("the audit log cannot be changed")
			}
			if err := c.App.Delete(r); err != nil {
				return nil, explain(err)
			}
			return "deleted " + r.Id, nil
		}})

	// ---- server-side JavaScript ----
	AddTool(&Tool{Name: "hooks_list", Scope: ScopeRead, Description: "The server-side JavaScript files (pb_hooks/*.pb.js).",
		Run: func(c *Call) (any, error) { return listDir(cfg.HooksDir, func(p string) bool { return strings.HasSuffix(p, ".pb.js") }) }})
	AddTool(&Tool{Name: "hook_read", Scope: ScopeRead, Description: "Read a hook file.",
		Parameters: obj(map[string]any{"name": str("file name, e.g. orders.pb.js")}, "name"),
		Run: func(c *Call) (any, error) {
			p, err := hookPath(c.Str("name"))
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("no hook %q", c.Str("name"))
			}
			return string(b), nil
		}})
	AddTool(&Tool{Name: "hook_write", Scope: ScopeHooks,
		Description: `Write server-side JavaScript (PocketBase JSVM). The server checks the syntax, saves it in pb_hooks and reloads. Examples:
routerAdd("GET", "/api/hello/{name}", (e) => e.json(200, { hi: e.request.pathValue("name") }))
onRecordCreate((e) => { e.record.set("slug", e.record.get("title").toLowerCase()); e.next() }, "posts")
cronAdd("nightly", "0 3 * * *", () => { $app.logger().info("tick") })
Use $app for the app (findRecordsByFilter, save, …), $http.send for HTTP, $os.getenv for environment variables.`,
		Parameters: obj(map[string]any{"name": str("file name ending in .pb.js"), "content": str("the JavaScript")}, "name", "content"),
		Run: func(c *Call) (any, error) {
			p, err := hookPath(c.Str("name"))
			if err != nil {
				return nil, err
			}
			src := c.Str("content")
			if len(src) > 1<<20 {
				return nil, errors.New("a hook may be at most 1 MB")
			}
			if _, err := goja.Compile(c.Str("name"), src, false); err != nil {
				return nil, fmt.Errorf("syntax error, nothing was saved: %v", err)
			}
			if err := os.MkdirAll(cfg.HooksDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("saved %s (%d bytes); the server reloads hooks when they change", c.Str("name"), len(src)), nil
		}})
	AddTool(&Tool{Name: "hook_delete", Scope: ScopeHooks, Destructive: true, Description: "Delete a hook file.",
		Parameters: obj(map[string]any{"name": str("file name")}, "name"),
		Run: func(c *Call) (any, error) {
			p, err := hookPath(c.Str("name"))
			if err != nil {
				return nil, err
			}
			if err := os.Remove(p); err != nil {
				return nil, fmt.Errorf("no hook %q", c.Str("name"))
			}
			return "deleted " + c.Str("name"), nil
		}})

	// ---- the static site ----
	AddTool(&Tool{Name: "site_list", Scope: ScopeRead, Description: "Files of the static site (pb_public, served at /).",
		Run: func(c *Call) (any, error) { return listDir(cfg.PublicDir, nil) }})
	AddTool(&Tool{Name: "site_read", Scope: ScopeRead, Description: "Read a site file (text; binary files come back as base64).",
		Parameters: obj(map[string]any{"path": str("e.g. index.html or css/app.css")}, "path"),
		Run: func(c *Call) (any, error) {
			p, err := sitePath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("no file %q", c.Str("path"))
			}
			if utf8.Valid(b) {
				return string(b), nil
			}
			return map[string]any{"base64": base64.StdEncoding.EncodeToString(b)}, nil
		}})
	AddTool(&Tool{Name: "site_write", Scope: ScopeSite,
		Description: "Write a file of the static site (pb_public). It is live at /<path> right away. For images and other binary files pass base64 instead of content.",
		Parameters:  obj(map[string]any{"path": str("e.g. index.html"), "content": str("text content"), "base64": str("binary content, base64")}, "path"),
		Run: func(c *Call) (any, error) {
			p, err := sitePath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			data := []byte(c.Str("content"))
			if b := c.Str("base64"); b != "" {
				if data, err = base64.StdEncoding.DecodeString(b); err != nil {
					return nil, fmt.Errorf("base64: %w", err)
				}
			}
			if len(data) > maxFile {
				return nil, fmt.Errorf("a site file may be at most %d MB", maxFile>>20)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("saved /%s (%d bytes)", filepath.ToSlash(c.Str("path")), len(data)), nil
		}})
	AddTool(&Tool{Name: "site_delete", Scope: ScopeSite, Destructive: true, Description: "Delete a file of the static site.",
		Parameters: obj(map[string]any{"path": str("file path")}, "path"),
		Run: func(c *Call) (any, error) {
			p, err := sitePath(c.Str("path"))
			if err != nil {
				return nil, err
			}
			if err := os.Remove(p); err != nil {
				return nil, fmt.Errorf("no file %q", c.Str("path"))
			}
			return "deleted " + c.Str("path"), nil
		}})

	// ---- settings and logs ----
	AddTool(&Tool{Name: "settings_get", Scope: ScopeSettings, Description: "The app settings (passwords and secrets are blanked).",
		Run: func(c *Call) (any, error) { return c.App.Settings(), nil }})
	AddTool(&Tool{Name: "settings_update", Scope: ScopeSettings, Destructive: true,
		Description: `Change settings. set is merged in, e.g. {"meta": {"appName": "Acme", "appURL": "https://acme.test"}}, {"smtp": {...}}, {"rateLimits": {...}}. Leaving a secret out keeps it.`,
		Parameters:  obj(map[string]any{"set": objp("settings to change")}, "set"),
		Run: func(c *Call) (any, error) {
			s, err := c.App.Settings().Clone()
			if err != nil {
				return nil, err
			}
			if err := applyJSON(s, c.Map("set")); err != nil {
				return nil, err
			}
			if err := c.App.Save(s); err != nil {
				return nil, explain(err)
			}
			return c.App.Settings(), nil
		}})
	AddTool(&Tool{Name: "logs_list", Scope: ScopeRead,
		Description: "Recent server logs (requests and app logs), newest first. level: minimum level (-4 debug, 0 info, 4 warn, 8 error).",
		Parameters:  obj(map[string]any{"limit": num("default 50, at most 500"), "level": num("minimum level"), "contains": str("only messages containing this")}),
		Run: func(c *Call) (any, error) {
			limit := c.Int("limit", 50)
			if limit < 1 || limit > 500 {
				limit = 500
			}
			q := c.App.LogQuery().OrderBy("created DESC").Limit(int64(limit))
			if _, ok := c.Args["level"]; ok {
				q = q.AndWhere(dbx.NewExp("level >= {:l}", dbx.Params{"l": c.Int("level", 0)}))
			}
			if s := c.Str("contains"); s != "" {
				q = q.AndWhere(dbx.Like("message", s))
			}
			logs := []*core.Log{}
			if err := q.All(&logs); err != nil {
				return nil, err
			}
			return logs, nil
		}})
	AddTool(&Tool{Name: "audit_list", Scope: ScopeRead, Description: "What agents did recently (the audit log), newest first.",
		Parameters: obj(map[string]any{"limit": num("default 50"), "tool": str("only this tool")}),
		Run: func(c *Call) (any, error) {
			f := ""
			params := dbx.Params{}
			if t := c.Str("tool"); t != "" {
				f = "tool = {:t}"
				params["t"] = t
			}
			rs, err := c.App.FindRecordsByFilter(AuditCollection, f, "-created", c.Int("limit", 50), 0, params)
			if err != nil {
				return nil, err
			}
			return export(rs), nil
		}})
}

// ---- helpers ----

func fieldRequired(f core.Field) (bool, bool) {
	b, err := json.Marshal(f)
	if err != nil {
		return false, false
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r, ok := m["required"].(bool)
	return r, ok
}

// applyJSON sets properties on a PocketBase model from a map, through its JSON form
func applyJSON(dst any, m map[string]any) error {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("invalid value: %v", err)
	}
	return nil
}

func addFields(app core.App, col *core.Collection, list []any) error {
	if len(list) == 0 {
		return nil
	}
	for i, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return fmt.Errorf("fields[%d] must be an object", i)
		}
		// relations may name the collection instead of its id
		if cid, ok := m["collectionId"].(string); ok {
			if rc, err := app.FindCollectionByNameOrId(cid); err == nil {
				m["collectionId"] = rc.Id
			} else if cid != col.Name {
				return fmt.Errorf("fields[%d]: no collection %q to relate to", i, cid)
			} else {
				m["collectionId"] = col.Id
			}
		}
		if n, _ := m["name"].(string); n != "" && col.Fields.GetByName(n) != nil {
			return fmt.Errorf("field %q already exists (use updateFields)", n)
		}
	}
	b, _ := json.Marshal(list)
	var fl core.FieldsList
	if err := json.Unmarshal(b, &fl); err != nil {
		return fmt.Errorf("fields: %v", err)
	}
	for _, f := range fl {
		col.Fields.Add(f)
	}
	return nil
}

func setAll(r *core.Record, data map[string]any) {
	for k, v := range data {
		r.Set(k, v)
	}
}

func export(rs []*core.Record) []map[string]any {
	out := make([]map[string]any, len(rs))
	for i, r := range rs {
		r.IgnoreEmailVisibility(true)
		out[i] = r.PublicExport()
	}
	return out
}

// explain turns validation errors into one readable line
func explain(err error) error {
	b, e := json.Marshal(err)
	if e == nil && len(b) > 2 && string(b) != "{}" && string(b) != "null" {
		return fmt.Errorf("%v %s", err, b)
	}
	return err
}

var hookName = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.pb\.js$`)

func hookPath(name string) (string, error) {
	if !hookName.MatchString(name) || strings.Contains(name, "..") {
		return "", errors.New("hook names look like orders.pb.js (letters, digits, _ . -)")
	}
	return filepath.Join(cfg.HooksDir, name), nil
}

func sitePath(p string) (string, error) {
	p = filepath.ToSlash(strings.TrimPrefix(p, "/"))
	if p == "" || strings.HasSuffix(p, "/") {
		return "", errors.New("give a file path, e.g. index.html")
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", errors.New("the path must stay inside the site")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if strings.HasPrefix(part, ".") {
			return "", errors.New("hidden files are not allowed")
		}
	}
	return filepath.Join(cfg.PublicDir, clean), nil
}

func listDir(dir string, keep func(string) bool) (any, error) {
	out := []map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || len(out) >= 2000 {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if keep != nil && !keep(rel) {
			return nil
		}
		info, _ := d.Info()
		out = append(out, map[string]any{"path": rel, "size": info.Size(), "modified": info.ModTime().UTC().Format("2006-01-02 15:04:05")})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i]["path"].(string) < out[j]["path"].(string) })
	return out, err
}
