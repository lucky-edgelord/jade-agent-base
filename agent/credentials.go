package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"
)

// Task credentials: short-lived tokens for one task, with fewer scopes than the agent that asked for them.
//
//	POST /api/agent/credentials   {"task_id": "flow-42", "scopes": ["read","write"], "ttl": "15m"}
//	→ {"token", "id", "task_id", "scopes", "expires"}
//	POST /api/agent/credentials/{id}/revoke
//
// A credential is a PocketBase token of the agent with a "jab_cred" claim. Each request with one looks the credential up:
// if it is missing, revoked, expired or cannot be read, the request is refused (fails closed). Its scopes are the
// credential's scopes that the agent still has. Every tool call made with it is audited with its task_id.
// A credential can mint narrower ones (never wider, never longer-lived than itself).

const (
	CredentialsCollection = "agent_credentials"
	credClaim             = "jab_cred"
	DefaultCredentialTTL  = 15 * time.Minute
	MaxCredentialTTL      = 24 * time.Hour
)

// ErrCredential is returned when a task credential is not (or no longer) valid.
var ErrCredential = errors.New("credential refused")

func ensureCredentials(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(CredentialsCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(CredentialsCollection)
	c.Fields.Add(
		&core.TextField{Name: "agent", Required: true},
		&core.TextField{Name: "task_id", Required: true, Max: 200},
		&core.JSONField{Name: "scopes"},
		&core.DateField{Name: "expires", Required: true},
		&core.BoolField{Name: "revoked"},
		&core.TextField{Name: "parent"},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	c.AddIndex("idx_agent_credentials_agent", false, "agent", "")
	return app.Save(c)
}

// credential reads the task credential a request carries, if any: nil when it is an ordinary token.
func credential(e *core.RequestEvent) (*core.Record, error) {
	h := e.Request.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "Bearer "), "bearer "))
	if tok == "" {
		return nil, nil
	}
	claims, err := security.ParseUnverifiedJWT(tok) // PocketBase has already verified the signature
	if err != nil {
		return nil, nil
	}
	id, _ := claims[credClaim].(string)
	if id == "" {
		return nil, nil
	}
	c, err := e.App.FindRecordById(CredentialsCollection, id)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown or unreadable task credential", ErrCredential)
	}
	switch {
	case e.Auth == nil || c.GetString("agent") != e.Auth.Id:
		return nil, fmt.Errorf("%w: the credential belongs to another agent", ErrCredential)
	case c.GetBool("revoked"):
		return nil, fmt.Errorf("%w: the task credential was revoked; stop the task and ask for a new one", ErrCredential)
	case !c.GetDateTime("expires").Time().After(time.Now()):
		return nil, fmt.Errorf("%w: the task credential expired", ErrCredential)
	}
	return c, nil
}

func credScopes(c *core.Record) []string {
	var l []string
	_ = c.UnmarshalJSONField("scopes", &l)
	return l
}

// ParseTTL reads "15m", "2h", "900" (seconds) or "" (the default), within the bounds.
func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultCredentialTTL, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		var n int
		if _, e := fmt.Sscanf(s, "%d", &n); e != nil || fmt.Sprint(n) != s {
			return 0, fmt.Errorf("ttl: use a duration like \"15m\" or seconds, not %q", s)
		}
		d = time.Duration(n) * time.Second
	}
	if d < 10*time.Second || d > MaxCredentialTTL {
		return 0, fmt.Errorf("ttl must be between 10s and %s", MaxCredentialTTL)
	}
	return d, nil
}

// MintCredential issues a task credential for an agent (see the package comment).
func MintCredential(app core.App, agentRec *core.Record, have map[string]bool, parent *core.Record, task string, scopes []string, ttl time.Duration) (map[string]any, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, errors.New("task_id is required: one credential per task, flow or scheduled job")
	}
	if len(scopes) == 0 {
		return nil, errors.New("scopes is required: name the fewest the task needs, e.g. [\"read\"]")
	}
	for _, s := range scopes {
		if !knownScope(s) {
			return nil, fmt.Errorf("unknown scope %q (scopes: %s)", s, strings.Join(AllScopes, ", "))
		}
		if !have[s] {
			return nil, fmt.Errorf("%w: a credential cannot have the %q scope its maker does not have", ErrScope, s)
		}
	}
	exp := time.Now().Add(ttl)
	if parent != nil {
		if pe := parent.GetDateTime("expires").Time(); exp.After(pe) {
			exp = pe
		}
	}
	col, err := app.FindCollectionByNameOrId(CredentialsCollection)
	if err != nil {
		return nil, err
	}
	c := core.NewRecord(col)
	c.Set("agent", agentRec.Id)
	c.Set("task_id", task)
	c.Set("scopes", scopes)
	dt, _ := types.ParseDateTime(exp)
	c.Set("expires", dt)
	if parent != nil {
		c.Set("parent", parent.Id)
	}
	if err := app.Save(c); err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{
		core.TokenClaimType: core.TokenTypeAuth, core.TokenClaimId: agentRec.Id, core.TokenClaimCollectionId: agentRec.Collection().Id,
		core.TokenClaimRefreshable: false, credClaim: c.Id, "jab_task": task,
	}
	tok, err := security.NewJWT(claims, agentRec.TokenKey()+agentRec.Collection().AuthToken.Secret, time.Until(exp))
	if err != nil {
		return nil, err
	}
	return map[string]any{"token": tok, "id": c.Id, "task_id": task, "scopes": scopes, "expires": exp.UTC().Format(time.RFC3339)}, nil
}

func mintRoute(e *core.RequestEvent) error {
	actor, have, task, err := CallerTask(e)
	if err != nil {
		return authError(e, err)
	}
	var b struct {
		TaskID string   `json:"task_id"`
		Scopes []string `json:"scopes"`
		TTL    any      `json:"ttl"`
		Agent  string   `json:"agent"` // superusers: for which agent
	}
	if err := e.BindBody(&b); err != nil {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": "the body must be JSON: " + err.Error()})
	}
	ttl, err := ParseTTL(strings.TrimSuffix(fmt.Sprint(nz(b.TTL)), ".000000"))
	if err != nil {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	agentRec := actor
	if actor.IsSuperuser() {
		if b.Agent == "" {
			return e.JSON(http.StatusBadRequest, map[string]any{"error": "superusers name the agent: {\"agent\": \"<id>\"}"})
		}
		if agentRec, err = e.App.FindRecordById(AgentsCollection, b.Agent); err != nil {
			return e.JSON(http.StatusNotFound, map[string]any{"error": "no agent " + b.Agent})
		}
		have = map[string]bool{}
		for _, s := range agentRec.GetStringSlice("scopes") {
			have[s] = true
		}
	}
	var parent *core.Record
	if task != "" {
		parent, _ = credential(e)
	}
	out, err := MintCredential(e.App, agentRec, have, parent, b.TaskID, b.Scopes, ttl)
	auditWith(e.App, actor, "credential_mint", map[string]any{"task_id": b.TaskID, "scopes": b.Scopes, "ttl": ttl.String(), "agent": agentRec.Id}, err, RunOpts{Task: task})
	if err != nil {
		return e.JSON(httpStatus(err), map[string]any{"error": err.Error()})
	}
	return e.JSON(http.StatusOK, out)
}

func revokeCredentialRoute(e *core.RequestEvent) error {
	actor, _, task, err := CallerTask(e)
	if err != nil {
		return authError(e, err)
	}
	c, err := e.App.FindRecordById(CredentialsCollection, e.Request.PathValue("id"))
	if err != nil || (!actor.IsSuperuser() && c.GetString("agent") != actor.Id) {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "no such credential"})
	}
	c.Set("revoked", true)
	err = e.App.Save(c)
	// credentials made from this one end with it
	if err == nil {
		kids, _ := e.App.FindAllRecords(CredentialsCollection)
		changed := true
		for changed {
			changed = false
			for _, k := range kids {
				if !k.GetBool("revoked") && k.GetString("parent") != "" {
					if p, e2 := e.App.FindRecordById(CredentialsCollection, k.GetString("parent")); e2 == nil && p.GetBool("revoked") {
						k.Set("revoked", true)
						_ = e.App.Save(k)
						changed = true
					}
				}
			}
		}
	}
	auditWith(e.App, actor, "credential_revoke", map[string]any{"id": c.Id, "task_id": c.GetString("task_id")}, err, RunOpts{Task: task})
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return e.JSON(http.StatusOK, map[string]any{"revoked": c.Id})
}

func nz(v any) any {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%d", int64(f))
	}
	return v
}

// authError answers a refused caller: 403 for a refused credential (stop, don't retry), 401 otherwise.
func authError(e *core.RequestEvent, err error) error {
	if errors.Is(err, ErrCredential) {
		return e.JSON(http.StatusForbidden, map[string]any{"error": err.Error(), "code": "credential_refused"})
	}
	return e.UnauthorizedError(err.Error(), nil)
}
