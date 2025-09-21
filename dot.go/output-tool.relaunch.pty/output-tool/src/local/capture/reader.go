package capture

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

func ReadAll(path string) ([]Rec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAllFromReader(f)
}

func ReadAllFromReader(r io.Reader) ([]Rec, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64*1024))
	var out []Rec
	for {
		var rec Rec
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
