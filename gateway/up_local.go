package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/lucky-edgelord/jade-agent-base/staticembed"
)

// ---- in the server: model2vec static embedding models (pure Go), downloaded from the Hub on first use ----

// where local models are kept (pb_data/models)
var localDir string

// SuggestedLocal are good static embedding models to start with.
var SuggestedLocal = []string{"minishlab/potion-base-8M", "minishlab/potion-base-4M", "minishlab/potion-base-32M", "minishlab/potion-retrieval-32M"}

type localUp struct{}

func (localUp) Chat(context.Context, *Provider, string, *ChatReq, func(Event) error) error {
	return errors.New("the local provider runs embedding models only")
}

func (localUp) Embed(ctx context.Context, p *Provider, model string, input []string) ([][]float64, int, error) {
	m, err := staticembed.Load(localDir, model)
	if err != nil {
		return nil, 0, &UpstreamError{Status: 502, Msg: err.Error()}
	}
	vecs, tokens := m.Embed(input)
	return vecs, tokens, nil
}

// the local models already downloaded
func localModels() []string {
	out := []string{}
	owners, _ := os.ReadDir(localDir)
	for _, o := range owners {
		names, _ := os.ReadDir(filepath.Join(localDir, o.Name()))
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(localDir, o.Name(), n.Name(), "model.safetensors")); err == nil {
				out = append(out, o.Name()+"/"+n.Name())
			}
		}
	}
	return out
}

func isLocalName(s string) bool { return strings.Count(s, "/") == 1 }
