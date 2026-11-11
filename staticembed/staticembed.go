// Package staticembed runs model2vec static embedding models (for example minishlab/potion-base-8M) in pure Go: a
// BERT WordPiece tokenizer, a matrix of token vectors, and their mean. No native libraries, no GPU; thousands of
// sentences a second on one core. Models download from the Hugging Face Hub the first time they are used.
package staticembed

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// HubURL is where models come from (tests point it elsewhere).
var HubURL = "https://huggingface.co"

// MaxTokens per text, as model2vec does.
const MaxTokens = 512

// Model is a loaded static embedding model.
type Model struct {
	Name      string
	Dim       int
	vecs      []float32 // vocab × Dim
	vocab     map[string]int
	unk       int
	normalize bool
	lower     bool
	strip     bool
	maxChars  int
}

var (
	mu     sync.Mutex
	loaded = map[string]*Model{}
	inprog = map[string]*sync.Mutex{}
)

// Load returns a model, downloading it into dir/<repo> the first time (and loading it into memory once).
func Load(dir, repo string) (*Model, error) {
	mu.Lock()
	if m := loaded[repo]; m != nil {
		mu.Unlock()
		return m, nil
	}
	l := inprog[repo]
	if l == nil {
		l = &sync.Mutex{}
		inprog[repo] = l
	}
	mu.Unlock()
	l.Lock()
	defer l.Unlock()
	mu.Lock()
	if m := loaded[repo]; m != nil {
		mu.Unlock()
		return m, nil
	}
	mu.Unlock()
	if strings.Contains(repo, "..") || strings.Count(repo, "/") != 1 {
		return nil, fmt.Errorf("model names look like owner/name, e.g. minishlab/potion-base-8M")
	}
	local := filepath.Join(dir, filepath.FromSlash(repo))
	for _, f := range []string{"config.json", "tokenizer.json", "model.safetensors"} {
		if err := fetch(repo, f, filepath.Join(local, f)); err != nil {
			return nil, err
		}
	}
	m, err := open(local, repo)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	loaded[repo] = m
	mu.Unlock()
	return m, nil
}

func fetch(repo, file, dst string) error {
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	res, err := http.Get(HubURL + "/" + repo + "/resolve/main/" + file)
	if err != nil {
		return fmt.Errorf("download %s/%s: %w", repo, file, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download %s/%s: HTTP %d (is it a model2vec model?)", repo, file, res.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(res.Body, 2<<30)); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("download %s/%s: %w", repo, file, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func open(dir, name string) (*Model, error) {
	var cfg struct {
		Normalize *bool `json:"normalize"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	var tok struct {
		Normalizer *struct {
			Type         string `json:"type"`
			Lowercase    bool   `json:"lowercase"`
			StripAccents *bool  `json:"strip_accents"`
		} `json:"normalizer"`
		Model struct {
			Type     string         `json:"type"`
			Unk      string         `json:"unk_token"`
			Prefix   string         `json:"continuing_subword_prefix"`
			MaxChars int            `json:"max_input_chars_per_word"`
			Vocab    map[string]int `json:"vocab"`
		} `json:"model"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if tok.Model.Type != "WordPiece" || (tok.Model.Prefix != "" && tok.Model.Prefix != "##") {
		return nil, fmt.Errorf("%s: only WordPiece tokenizers are supported (this one is %s)", name, tok.Model.Type)
	}
	m := &Model{Name: name, vocab: tok.Model.Vocab, unk: -1, normalize: cfg.Normalize == nil || *cfg.Normalize, maxChars: tok.Model.MaxChars}
	if m.maxChars == 0 {
		m.maxChars = 100
	}
	if id, ok := m.vocab[tok.Model.Unk]; ok {
		m.unk = id
	}
	if n := tok.Normalizer; n != nil && n.Type == "BertNormalizer" {
		m.lower = n.Lowercase
		m.strip = n.Lowercase
		if n.StripAccents != nil {
			m.strip = *n.StripAccents
		}
	}
	m.vecs, m.Dim, err = readSafetensors(filepath.Join(dir, "model.safetensors"), len(m.vocab))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// the "embeddings" tensor (F32 or F16) of a safetensors file
func readSafetensors(path string, vocab int) ([]float32, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil || n > 1<<24 {
		return nil, 0, errors.New("not a safetensors file")
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, 0, err
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &h); err != nil {
		return nil, 0, err
	}
	var t struct {
		Dtype   string  `json:"dtype"`
		Shape   []int   `json:"shape"`
		Offsets []int64 `json:"data_offsets"`
	}
	raw, ok := h["embeddings"]
	if !ok {
		return nil, 0, errors.New("no embeddings tensor in model.safetensors")
	}
	if err := json.Unmarshal(raw, &t); err != nil || len(t.Shape) != 2 || len(t.Offsets) != 2 {
		return nil, 0, errors.New("unexpected embeddings tensor")
	}
	if _, ok := h["mapping"]; ok {
		return nil, 0, errors.New("quantized (vocabulary-mapped) models are not supported yet")
	}
	rows, dim := t.Shape[0], t.Shape[1]
	if rows < vocab {
		return nil, 0, fmt.Errorf("the model has %d rows for %d tokens", rows, vocab)
	}
	size := t.Offsets[1] - t.Offsets[0]
	if _, err := f.Seek(8+int64(n)+t.Offsets[0], io.SeekStart); err != nil {
		return nil, 0, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, 0, err
	}
	out := make([]float32, rows*dim)
	switch t.Dtype {
	case "F32":
		if int64(len(out))*4 != size {
			return nil, 0, errors.New("size mismatch")
		}
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
		}
	case "F16":
		if int64(len(out))*2 != size {
			return nil, 0, errors.New("size mismatch")
		}
		for i := range out {
			out[i] = f16(binary.LittleEndian.Uint16(data[2*i:]))
		}
	default:
		return nil, 0, fmt.Errorf("unsupported dtype %s", t.Dtype)
	}
	return out, dim, nil
}

func f16(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := int((h >> 10) & 0x1f)
	frac := uint32(h & 0x3ff)
	switch exp {
	case 0:
		if frac == 0 {
			return math.Float32frombits(sign)
		}
		v := float32(frac) / 1024 * float32(math.Pow(2, -14))
		if sign != 0 {
			return -v
		}
		return v
	case 31:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | uint32(exp-15+127)<<23 | frac<<13)
}

// ---- tokenizing (BERT: normalize, split on whitespace and punctuation, WordPiece) ----

func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x20000 && r <= 0x2A6DF) || (r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) || (r >= 0x2B820 && r <= 0x2CEAF) || (r >= 0xF900 && r <= 0xFAFF) || (r >= 0x2F800 && r <= 0x2FA1F)
}

func (m *Model) normalizeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0 || r == 0xFFFD || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'):
			continue
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case isCJK(r):
			b.WriteRune(' ')
			b.WriteRune(r)
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if m.strip {
		var c strings.Builder
		for _, r := range norm.NFD.String(out) {
			if !unicode.Is(unicode.Mn, r) {
				c.WriteRune(r)
			}
		}
		out = c.String()
	}
	if m.lower {
		out = strings.ToLower(out)
	}
	return out
}

// Tokenize returns the token ids of a text (no special tokens; unknown words left out), at most MaxTokens.
func (m *Model) Tokenize(s string) []int {
	ids := []int{}
	for _, field := range strings.Fields(m.normalizeText(s)) {
		start := 0
		rs := []rune(field)
		for i, r := range rs {
			if isPunct(r) {
				if i > start {
					ids = m.wordpiece(string(rs[start:i]), ids)
				}
				ids = m.wordpiece(string(r), ids)
				start = i + 1
			}
		}
		if start < len(rs) {
			ids = m.wordpiece(string(rs[start:]), ids)
		}
		if len(ids) >= MaxTokens*2 {
			break
		}
	}
	out := ids[:0]
	for _, id := range ids {
		if id != m.unk {
			out = append(out, id)
		}
	}
	if len(out) > MaxTokens {
		out = out[:MaxTokens]
	}
	return out
}

func (m *Model) wordpiece(word string, ids []int) []int {
	rs := []rune(word)
	if len(rs) > m.maxChars {
		return append(ids, m.unk)
	}
	var pieces []int
	for start := 0; start < len(rs); {
		end := len(rs)
		found := -1
		for end > start {
			sub := string(rs[start:end])
			if start > 0 {
				sub = "##" + sub
			}
			if id, ok := m.vocab[sub]; ok {
				found = id
				break
			}
			end--
		}
		if found < 0 {
			return append(ids, m.unk)
		}
		pieces = append(pieces, found)
		start = end
	}
	return append(ids, pieces...)
}

// Embed returns one vector per text: the mean of its tokens' vectors (normalized when the model says so).
func (m *Model) Embed(texts []string) ([][]float64, int) {
	out := make([][]float64, len(texts))
	tokens := 0
	for i, t := range texts {
		ids := m.Tokenize(t)
		tokens += len(ids)
		v := make([]float64, m.Dim)
		for _, id := range ids {
			row := m.vecs[id*m.Dim : (id+1)*m.Dim]
			for j, x := range row {
				v[j] += float64(x)
			}
		}
		if n := len(ids); n > 0 {
			for j := range v {
				v[j] /= float64(n)
			}
		}
		if m.normalize {
			var s float64
			for _, x := range v {
				s += x * x
			}
			if s = math.Sqrt(s); s > 0 {
				for j := range v {
					v[j] /= s
				}
			}
		}
		out[i] = v
	}
	return out, tokens
}
