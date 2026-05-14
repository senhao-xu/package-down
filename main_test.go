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

func TestNoResolvedFilesReturnsZipManifest(t *testing.T) {
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}

	if !zipHasFile(reader, "README.txt") {
		t.Fatalf("expected README.txt in zip, got %#v", zipNames(reader))
	}
	manifestBody, ok := readZipFile(t, reader, "manifest.json")
	if !ok {
		t.Fatalf("expected manifest.json in zip, got %#v", zipNames(reader))
	}
	if !strings.Contains(manifestBody, "direct package url download is disabled") {
		t.Fatalf("expected manifest error, got %q", manifestBody)
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

func readZipFile(t *testing.T, reader *zip.Reader, name string) (string, bool) {
	t.Helper()

	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		body, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data), true
	}
	return "", false
}
