package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ", ")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type fileEntry struct {
	Filename      string `json:"filename"`
	MimeType      string `json:"mime_type"`
	SizeBytes     int64  `json:"size_bytes"`
	Sha256Bytes   string `json:"sha256_bytes"`
	B64Sha256     string `json:"b64_sha256"`
	ContentBase64 string `json:"content_base64"`
}

var (
	useGit   = flag.Bool("git", true, "Apply gitignore rules (only valid for directories)")
	isFile   = flag.Bool("file", false, "Treat path as a single file")
	note     = flag.String("note", "Decode and verify the sha256sum of these files, analyze them, and we will discuss them", "Note to include in output")
	output   = flag.String("output", "", "Output file path (default: stdout)")
	recurse  = flag.Bool("recurse", true, "Recurse into directories (only valid for directories)")
	verify   = flag.Bool("verify", true, "Verify output file contents (only if --output is specified)")
	excludes stringSliceFlag
)

func main() {
	flag.Var(&excludes, "exclude", "Exclude specific file or directory (can be repeated)")
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Error: Exactly one path argument is required.")
		os.Exit(1)
	}
	root := args[0]

	var out io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f

		absOut, _ := filepath.Abs(*output)
		absRoot, _ := filepath.Abs(root)
		if strings.HasPrefix(absOut, absRoot) {
			excludes = append(excludes, filepath.ToSlash(absOut))
		}
	}

	files := []string{}
	var err error
	if *isFile {
		files = []string{root}
	} else {
		if *useGit {
			files, err = getGitIncludedFiles(root, *recurse, excludes)
		} else {
			files, err = walkFiles(root, *recurse, excludes)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error collecting files: %v\n", err)
			os.Exit(1)
		}
	}

	sort.Strings(files)

	manifest := map[string]string{
		"format": "filedigest/v2",
		"note":   *note,
	}
	json.NewEncoder(out).Encode(manifest)

	schema := map[string]string{
		"filename":       "relative path to file",
		"mime_type":      "detected MIME type",
		"size_bytes":     "size of original file in bytes",
		"sha256_bytes":   "SHA256 of raw file bytes",
		"b64_sha256":     "SHA256 of base64-encoded content",
		"content_base64": "base64-encoded file content",
	}
	json.NewEncoder(out).Encode(map[string]interface{}{"schema": schema})

	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)

		mime := detectMimeType(f)
		if !isTextFile(f, mime) {
			continue
		}

		entry := fileEntry{
			Filename: rel,
			MimeType: mime,
		}

		file, err := os.Open(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening file %q: %v\n", f, err)
			continue
		}
		defer file.Close()

		h := sha256.New()
		var b64Buf bytes.Buffer
		b64Enc := base64.NewEncoder(base64.StdEncoding, &b64Buf)
		r := io.TeeReader(file, h)

		n, err := io.Copy(b64Enc, r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding file %q: %v\n", f, err)
			continue
		}
		b64Enc.Close()

		entry.SizeBytes = n
		entry.Sha256Bytes = fmt.Sprintf("%x", h.Sum(nil))
		entry.ContentBase64 = b64Buf.String()

		b64Hash := sha256.Sum256([]byte(entry.ContentBase64))
		entry.B64Sha256 = fmt.Sprintf("%x", b64Hash)

		json.NewEncoder(out).Encode(entry)
	}

	if *verify && *output != "" {
		verifyOutput(*output)
	}
}

func walkFiles(root string, recurse bool, excludes []string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, ex := range excludes {
			if strings.HasPrefix(rel, ex) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if info.IsDir() {
			if !recurse && p != root {
				return filepath.SkipDir
			}
			if filepath.Base(p) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, p)
		return nil
	})
	return files, err
}

func getGitIncludedFiles(root string, recurse bool, excludes []string) ([]string, error) {
	var candidates []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, ex := range excludes {
			if strings.HasPrefix(rel, ex) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if info.IsDir() {
			if !recurse && p != root {
				return filepath.SkipDir
			}
			if filepath.Base(p) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		candidates = append(candidates, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("git", "-C", root, "check-ignore", "--stdin")
	stdin, _ := cmd.StdinPipe()
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Start()
	for _, p := range candidates {
		fmt.Fprintln(stdin, p)
	}
	stdin.Close()
	_ = cmd.Wait()

	ignored := map[string]bool{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		ignored[scanner.Text()] = true
	}

	var included []string
	for _, p := range candidates {
		if !ignored[p] {
			included = append(included, filepath.Join(root, p))
		}
	}
	return included, nil
}

func detectMimeType(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "application/octet-stream"
	}
	return http.DetectContentType(buf[:n])
}

func isTextFile(path string, mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return utf8.Valid(data)
}

func verifyOutput(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file for verification: %v\n", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			var entry fileEntry
			if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Filename != "" {
				raw, err := os.ReadFile(entry.Filename)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Verification failed: cannot read %q\n", entry.Filename)
					continue
				}
				sum := sha256.Sum256(raw)
				if fmt.Sprintf("%x", sum) != entry.Sha256Bytes {
					fmt.Fprintf(os.Stderr, "Verification failed: sha256 mismatch for %q\n", entry.Filename)
				}
			}
		}
	}
}
