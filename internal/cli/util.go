package cli

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

func mimeByExt(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return http.DetectContentType(buf[:n])
}
