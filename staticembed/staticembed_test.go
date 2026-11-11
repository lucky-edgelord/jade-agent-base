package staticembed

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go model against the Python model2vec library on the same sentences (testdata/ref.json, made with model2vec
// 0.7.0 and minishlab/potion-base-8M). The model files are served from $JAB_M2V_DIR, or downloaded from the Hub.
func TestMatchesModel2Vec(t *testing.T) {
	var ref struct {
		S   []string    `json:"s"`
		V   [][]float64 `json:"v"`
		IDs [][]int     `json:"ids"`
	}
	b, err := os.ReadFile("testdata/ref.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &ref)
	if d := os.Getenv("JAB_M2V_DIR"); d != "" { // serve local copies as if they were the Hub
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filepath.Join(d, filepath.Base(r.URL.Path)))
		}))
		defer srv.Close()
		old := HubURL
		HubURL = srv.URL
		defer func() { HubURL = old }()
	}
	m, err := Load(t.TempDir(), "minishlab/potion-base-8M")
	if err != nil {
		t.Fatal(err)
	}
	if m.Dim != 256 {
		t.Fatalf("dim %d", m.Dim)
	}
	got, _ := m.Embed(ref.S)
	for i, s := range ref.S {
		ids := m.Tokenize(s)
		if mustJSON(ids) != mustJSON(trim(ref.IDs[i], m.unk)) {
			t.Errorf("tokens of %q:\n go %v\n py %v", s, ids, ref.IDs[i])
		}
		for j := range got[i] {
			if math.Abs(got[i][j]-ref.V[i][j]) > 1e-5 {
				t.Errorf("vector of %q differs at %d: %v vs %v", s, j, got[i][j], ref.V[i][j])
				break
			}
		}
	}
	// the same model is loaded once
	m2, _ := Load(t.TempDir(), "minishlab/potion-base-8M")
	if m2 != m {
		t.Error("loaded twice")
	}
	if _, err := Load(t.TempDir(), "../etc/passwd"); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("bad name: %v", err)
	}
}

func trim(ids []int, unk int) []int {
	out := []int{}
	for _, id := range ids {
		if id != unk {
			out = append(out, id)
		}
	}
	return out
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func BenchmarkEmbed(b *testing.B) {
	m, err := Load(b.TempDir(), "minishlab/potion-base-8M")
	if err != nil {
		b.Skip(err)
	}
	s := []string{"The quick brown fox jumps over the lazy dog, then naps in the warm afternoon sun."}
	for i := 0; i < b.N; i++ {
		m.Embed(s)
	}
}
