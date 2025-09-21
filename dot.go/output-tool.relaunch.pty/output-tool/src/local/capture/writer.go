package capture

import (
	"bufio"
	"encoding/json"
	"os"
)

type Rec struct {
	N    int    `json:"n"`
	Text string `json:"text"`
	M    bool   `json:"m"`
}

type Writer struct {
	f   *os.File
	bw  *bufio.Writer
	enc *json.Encoder
}

func NewTempWriter(prefix string) (*Writer, error) {
	f, err := os.CreateTemp(os.TempDir(), prefix+"*.jsonl")
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	enc := json.NewEncoder(bw)
	w := &Writer{f: f, bw: bw, enc: enc}
	return w, nil
}

func (w *Writer) Writer() *bufio.Writer { return w.bw }
func (w *Writer) Path() string          { return w.f.Name() }

func (w *Writer) Encode(rec *Rec) error {
	return w.enc.Encode(rec)
}

func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	if w.bw != nil {
		_ = w.bw.Flush()
	}
	if w.f != nil {
		return w.f.Close()
	}
	return nil
}
