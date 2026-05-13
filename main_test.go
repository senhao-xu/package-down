package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDirectPackageDownloadWritesZipFile(t *testing.T) {
	config = appConfig{
		DefaultProfileID: "almalinux-9",
		CacheTTL:         time.Minute,
		MaxPackages:      50,
		MaxResolved:      50,
		RequestTimeout:   time.Minute,
		AllowDirectURL:   true,
	}

	packageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-rpm")
		_, _ = w.Write([]byte("fake rpm payload"))
	}))
	defer packageServer.Close()

	form := url.Values{
		"packageName": {packageServer.URL + "/unzip.rpm"},
		"includeDeps": {"true"},
	}
	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()

	handleDownload(recorder, req)

	resp := recorder.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "unzip.zip") {
		t.Fatalf("expected zip filename to use package name, got %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}

	if !zipHasFile(reader, "rpms/unzip.rpm") {
		t.Fatalf("expected rpms/unzip.rpm in zip, got %#v", zipNames(reader))
	}
}

func TestNoResolvedFilesReturnsTextError(t *testing.T) {
	config = appConfig{
		DefaultProfileID: "almalinux-9",
		CacheTTL:         time.Minute,
		MaxPackages:      50,
		MaxResolved:      50,
		RequestTimeout:   time.Minute,
		AllowDirectURL:   false,
	}

	form := url.Values{
		"packageName": {"https://example.com/unzip.rpm"},
	}
	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()

	handleDownload(recorder, req)

	resp := recorder.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "没有找到可下载的包文件") {
		t.Fatalf("expected text error, got %q", string(body))
	}
}

func zipHasFile(reader *zip.Reader, name string) bool {
	for _, file := range reader.File {
		if file.Name == name {
			return true
		}
	}
	return false
}

func zipNames(reader *zip.Reader) []string {
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	return names
}
