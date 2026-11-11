package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Idempotency keys: a mutating tool call sent again with the same key (REST: the Idempotency-Key header; MCP:
// params._meta.idempotency_key) is not run again. The first answer, result or error, comes back again
// (REST: the Idempotent-Replayed: true header; MCP: _meta.replayed). Keys belong to the caller and last 24 hours.
// The same key with other arguments is refused (422); while the first call runs, the same key gets 409.
// Read tools ignore keys: reading twice is safe.

const (
	IdempotencyCollection = "agent_idempotency"
	IdempotencyTTL        = 24 * time.Hour
)

var (
	ErrKeyReused   = errors.New("idempotency key reused with different arguments")
	ErrKeyInFlight = errors.New("a call with this idempotency key is still running")
)

// ReplayedError is the stored error of the first call with a key.
type ReplayedError struct {
	Msg    string
	Status int
}

func (e *ReplayedError) Error() string { return e.Msg }

func ensureIdempotency(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(IdempotencyCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(IdempotencyCollection)
	c.Fields.Add(
		&core.TextField{Name: "actor", Required: true},
		&core.TextField{Name: "key", Required: true, Max: 200},
		&core.TextField{Name: "tool"},
		&core.TextField{Name: "hash"},
		&core.TextField{Name: "status"}, // running | done | failed
		&core.NumberField{Name: "code"},
		&core.JSONField{Name: "response", MaxSize: 8 << 20},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	c.AddIndex("idx_agent_idem_key", true, "actor, key", "")
	c.AddIndex("idx_agent_idem_created", false, "created", "")
	return app.Save(c)
}

func argsHash(tool string, args map[string]any) string {
	b, _ := json.Marshal(args) // map keys are sorted: the same arguments give the same hash
	h := sha256.Sum256(append([]byte(tool+"\x00"), b...))
	return hex.EncodeToString(h[:])
}

// RunIdempotent runs a tool once per (caller, key). replayed is true when the answer is the stored one.
func RunIdempotent(app core.App, actor *core.Record, scopes map[string]bool, name string, args map[string]any, o RunOpts) (out any, err error, replayed bool) {
	o.Key = strings.TrimSpace(o.Key)
	t := getTool(name)
	if o.Key == "" || t == nil || t.Scope == ScopeRead {
		out, err = RunWith(app, actor, scopes, name, args, o)
		return out, err, false
	}
	if len(o.Key) > 200 {
		return nil, errors.New("the idempotency key is longer than 200 characters"), false
	}
	if args == nil {
		args = map[string]any{}
	}
	col, cerr := app.FindCachedCollectionByNameOrId(IdempotencyCollection)
	if cerr != nil {
		return nil, cerr, false
	}
	who := actor.Collection().Name + "/" + actor.Id
	hash := argsHash(name, args)
	// forget keys older than a day
	_, _ = app.DB().NewQuery("DELETE FROM " + IdempotencyCollection + " WHERE created < {:t}").Bind(dbx.Params{"t": time.Now().Add(-IdempotencyTTL).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute()

	rec := core.NewRecord(col)
	rec.Set("actor", who)
	rec.Set("key", o.Key)
	rec.Set("tool", name)
	rec.Set("hash", hash)
	rec.Set("status", "running")
	if saveErr := app.Save(rec); saveErr != nil {
		// the key is taken: replay, or refuse
		prev, ferr := app.FindFirstRecordByFilter(IdempotencyCollection, "actor = {:a} && key = {:k}", dbx.Params{"a": who, "k": o.Key})
		if ferr != nil {
			return nil, fmt.Errorf("idempotency: %w", saveErr), false
		}
		if prev.GetString("hash") != hash {
			err = fmt.Errorf("%w (first used for %s)", ErrKeyReused, prev.GetString("tool"))
			auditWith(app, actor, name, args, err, o)
			return nil, err, false
		}
		if prev.GetString("status") == "running" {
			return nil, ErrKeyInFlight, false
		}
		o.Replayed = true
		var resp map[string]any
		_ = prev.UnmarshalJSONField("response", &resp)
		if prev.GetString("status") == "failed" {
			err = &ReplayedError{Msg: fmt.Sprint(resp["error"]), Status: prev.GetInt("code")}
		} else {
			out = resp["result"]
		}
		auditWith(app, actor, name, args, err, o)
		return out, err, true
	}
	out, err = RunWith(app, actor, scopes, name, args, o)
	if err != nil {
		rec.Set("status", "failed")
		rec.Set("code", httpStatus(err))
		rec.Set("response", map[string]any{"error": err.Error()})
	} else {
		rec.Set("status", "done")
		// store the answer as the client saw it (JSON), so a replay is the same bytes
		b, _ := json.Marshal(map[string]any{"result": out})
		var norm map[string]any
		_ = json.Unmarshal(b, &norm)
		rec.Set("response", norm)
	}
	if serr := app.Save(rec); serr != nil {
		app.Logger().Warn("agent idempotency", "error", serr)
	}
	return out, err, false
}

func idempotencyStatus(err error) int {
	var re *ReplayedError
	switch {
	case errors.As(err, &re) && re.Status > 0:
		return re.Status
	case errors.Is(err, ErrKeyReused):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrKeyInFlight):
		return http.StatusConflict
	}
	return 0
}
