package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// a raw request with extra headers → status, headers, body
func (e *env) raw(method, path, token string, hdr map[string]string, body any) (int, http.Header, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	q, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(b))
	q.Header.Set("Content-Type", "application/json")
	if token != "" {
		q.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		q.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(q)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	m := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, res.Header, m
}

func (e *env) agentToken(name string, scopes ...string) string {
	_, m, _ := e.req("POST", "/api/agent/agents", e.su, map[string]any{"name": name, "scopes": scopes})
	return m["token"].(string)
}

func TestTaskCredentials(t *testing.T) {
	e := setup(t)
	tok := e.agentToken("Booker", "read", "write", "schema")

	// mint: least privilege, bounded TTL, task id required
	code, _, m := e.raw("POST", "/api/agent/credentials", tok, nil, map[string]any{"task_id": "flow-42", "scopes": []string{"read"}, "ttl": "5m"})
	if code != 200 || m["token"] == nil || m["task_id"] != "flow-42" {
		t.Fatalf("mint: %d %v", code, m)
	}
	cred, credID := m["token"].(string), m["id"].(string)
	for _, bad := range []map[string]any{
		{"scopes": []string{"read"}},                                  // no task
		{"task_id": "x"},                                              // no scopes
		{"task_id": "x", "scopes": []string{"settings"}},              // a scope the agent lacks
		{"task_id": "x", "scopes": []string{"read"}, "ttl": "48h"},    // too long
		{"task_id": "x", "scopes": []string{"read"}, "ttl": "banana"}, // not a duration
	} {
		if c, _, mm := e.raw("POST", "/api/agent/credentials", tok, nil, bad); c == 200 {
			t.Fatalf("minted with %v: %v", bad, mm)
		}
	}
	// the credential reads, cannot write; the audit log has the task
	if c, _, mm := e.raw("POST", "/api/agent/tools/collections_list", cred, nil, map[string]any{}); c != 200 {
		t.Fatalf("read with credential: %d %v", c, mm)
	}
	if c, _, _ := e.raw("POST", "/api/agent/tools/collection_create", cred, nil, map[string]any{"name": "nope", "fields": []any{}}); c != 403 {
		t.Fatalf("write with a read credential: %d", c)
	}
	recs, _ := e.app.FindRecordsByFilter(AuditCollection, "task = 'flow-42'", "", 0, 0)
	if len(recs) < 2 {
		t.Fatalf("audit with task: %d records", len(recs))
	}
	// tools/list over MCP shows only what the credential allows
	_, _, mm := e.raw("POST", "/api/agent/mcp", cred, nil, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if s := mustJSON(mm); strings.Contains(s, `"collection_create"`) || !strings.Contains(s, `"collections_list"`) {
		t.Fatalf("mcp tools/list with a read credential: %.300s", s)
	}
	// a credential cannot widen itself, and its children end with it
	if c, _, _ := e.raw("POST", "/api/agent/credentials", cred, nil, map[string]any{"task_id": "child", "scopes": []string{"write"}}); c != 403 {
		t.Fatalf("a read credential minted write: %d", c)
	}
	_, _, cm := e.raw("POST", "/api/agent/credentials", cred, nil, map[string]any{"task_id": "child", "scopes": []string{"read"}, "ttl": "1h"})
	child := cm["token"].(string)
	if exp, _ := time.Parse(time.RFC3339, cm["expires"].(string)); exp.After(time.Now().Add(6 * time.Minute)) {
		t.Fatalf("a child outlives its parent: %v", cm["expires"])
	}
	// revoke: fails closed with 403 and a code, for the credential and its child
	if c, _, mm := e.raw("POST", "/api/agent/credentials/"+credID+"/revoke", tok, nil, nil); c != 200 {
		t.Fatalf("revoke: %d %v", c, mm)
	}
	for _, x := range []string{cred, child} {
		c, _, mm := e.raw("POST", "/api/agent/tools/collections_list", x, nil, map[string]any{})
		if c != 403 || mm["code"] != "credential_refused" {
			t.Fatalf("a revoked credential: %d %v", c, mm)
		}
		if c, _, _ := e.raw("POST", "/api/agent/mcp", x, nil, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); c != 403 {
			t.Fatalf("a revoked credential over MCP: %d", c)
		}
	}
	if c, _, _ := e.raw("GET", "/api/agent/tools", cred, nil, nil); c != 403 {
		t.Fatalf("tools list with a revoked credential: %d", c)
	}
	// the agent's own token still works; deleting the credential record also refuses (fails closed)
	if c, _, _ := e.raw("POST", "/api/agent/tools/collections_list", tok, nil, map[string]any{}); c != 200 {
		t.Fatalf("the agent token after revoking a credential: %d", c)
	}
	_, _, m2 := e.raw("POST", "/api/agent/credentials", tok, nil, map[string]any{"task_id": "t2", "scopes": []string{"read"}})
	r, _ := e.app.FindRecordById(CredentialsCollection, m2["id"].(string))
	_ = e.app.Delete(r)
	if c, _, _ := e.raw("POST", "/api/agent/tools/collections_list", m2["token"].(string), nil, map[string]any{}); c != 403 {
		t.Fatalf("a credential whose record is gone: %d", c)
	}
	// revoking the agent ends its credentials too (the signing key changes)
	_, _, m3 := e.raw("POST", "/api/agent/credentials", tok, nil, map[string]any{"task_id": "t3", "scopes": []string{"read"}})
	ag, _ := e.app.FindFirstRecordByData(AgentsCollection, "name", "Booker")
	_ = Revoke(e.app, ag)
	if c, _, _ := e.raw("POST", "/api/agent/tools/collections_list", m3["token"].(string), nil, map[string]any{}); c != 401 {
		t.Fatalf("a credential of a revoked agent: %d", c)
	}
	// another agent cannot revoke it
	other := e.agentToken("Other", "read")
	if c, _, _ := e.raw("POST", "/api/agent/credentials/"+m3["id"].(string)+"/revoke", other, nil, nil); c != 404 {
		t.Fatalf("another agent revoked a credential: %d", c)
	}
}

func TestIdempotency(t *testing.T) {
	e := setup(t)
	tok := e.agentToken("Booker", "read", "write", "schema")
	create := map[string]any{"name": "bookings", "fields": []any{map[string]any{"name": "title", "type": "text"}}}
	k := map[string]string{"Idempotency-Key": "018f-create-bookings"}
	c1, h1, m1 := e.raw("POST", "/api/agent/tools/collection_create", tok, k, create)
	c2, h2, m2 := e.raw("POST", "/api/agent/tools/collection_create", tok, k, create)
	if c1 != 200 || c2 != 200 || h1.Get("Idempotent-Replayed") != "" || h2.Get("Idempotent-Replayed") != "true" || mustJSON(m1) != mustJSON(m2) {
		t.Fatalf("replay: %d %d %q %v | %v", c1, c2, h2.Get("Idempotent-Replayed"), m1, m2)
	}
	// without a key the second create fails: the collection exists (so the key really prevented a second run)
	if c, _, _ := e.raw("POST", "/api/agent/tools/collection_create", tok, nil, create); c == 200 {
		t.Fatal("a second create without a key succeeded")
	}
	// the same key with other arguments is refused
	if c, _, m := e.raw("POST", "/api/agent/tools/collection_create", tok, k, map[string]any{"name": "other", "fields": []any{}}); c != 422 || !strings.Contains(m["error"].(string), "different arguments") {
		t.Fatalf("key reuse: %d %v", c, m)
	}
	// errors replay too
	bad := map[string]string{"Idempotency-Key": "bad-1"}
	ca, _, ma := e.raw("POST", "/api/agent/tools/record_create", tok, bad, map[string]any{"collection": "missing", "data": map[string]any{}})
	cb, hb, mb := e.raw("POST", "/api/agent/tools/record_create", tok, bad, map[string]any{"collection": "missing", "data": map[string]any{}})
	if ca == 200 || cb != ca || ma["error"] != mb["error"] || hb.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("error replay: %d %d %v %v", ca, cb, ma, mb)
	}
	// keys belong to the caller
	other := e.agentToken("Other", "read", "write", "schema")
	if c, h, _ := e.raw("POST", "/api/agent/tools/collection_create", other, k, map[string]any{"name": "theirs", "fields": []any{}}); c != 200 || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("another agent's same key: %d", c)
	}
	// concurrent duplicates: exactly one record
	var wg sync.WaitGroup
	rec := map[string]any{"collection": "bookings", "data": map[string]any{"title": "once"}}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.raw("POST", "/api/agent/tools/record_create", tok, map[string]string{"Idempotency-Key": "race"}, rec)
		}()
	}
	wg.Wait()
	if n, _ := e.app.CountRecords("bookings"); n != 1 {
		t.Fatalf("concurrent duplicates made %d records", n)
	}
	// MCP: _meta.idempotency_key
	call := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "record_create", "arguments": map[string]any{"collection": "bookings", "data": map[string]any{"title": "mcp"}}, "_meta": map[string]any{"idempotency_key": "mcp-1"}}}
	_, _, r1 := e.raw("POST", "/api/agent/mcp", tok, nil, call)
	_, _, r2 := e.raw("POST", "/api/agent/mcp", tok, nil, call)
	if !strings.Contains(mustJSON(r2), `"replayed":true`) || strings.Contains(mustJSON(r1), `"replayed":true`) {
		t.Fatalf("mcp replay: %v | %v", r1, r2)
	}
	if n, _ := e.app.CountRecords("bookings"); n != 2 {
		t.Fatalf("after the MCP pair: %d records", n)
	}
	// reads ignore keys; the audit log has keys and replays
	e.raw("POST", "/api/agent/tools/collections_list", tok, map[string]string{"Idempotency-Key": "r"}, map[string]any{})
	e.raw("POST", "/api/agent/tools/collections_list", tok, map[string]string{"Idempotency-Key": "r"}, map[string]any{})
	if recs, _ := e.app.FindRecordsByFilter(AuditCollection, "idempotency_key = '018f-create-bookings' && replayed = true", "", 0, 0); len(recs) != 1 {
		t.Fatalf("audit replays: %d", len(recs))
	}
}
