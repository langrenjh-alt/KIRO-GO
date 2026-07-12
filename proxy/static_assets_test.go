package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAdminAssets(t *testing.T) {
	handler := &Handler{}
	tests := []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/admin", contentType: "text/html", contains: "<!DOCTYPE html>"},
		{path: "/admin/", contentType: "text/html", contains: "<!DOCTYPE html>"},
		{path: "/admin/app.js", contentType: "javascript", contains: "drawTrafficChart"},
		{path: "/admin/styles.css", contentType: "text/css", contains: ":root"},
		{path: "/admin/locales/zh.json", contentType: "application/json", contains: `"traffic.title"`},
		{path: "/admin/vendor/fontawesome/webfonts/fa-solid-900.woff2", contentType: "font/woff2"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, test.contentType) {
				t.Fatalf("Content-Type = %q, want it to contain %q", got, test.contentType)
			}
			if recorder.Body.Len() == 0 {
				t.Fatal("embedded response body is empty")
			}
			if test.contains != "" && !strings.Contains(recorder.Body.String(), test.contains) {
				t.Fatalf("response does not contain %q", test.contains)
			}
		})
	}
}

func TestEmbeddedAdminAssetsDoNotExposeSourceFiles(t *testing.T) {
	handler := &Handler{}
	for _, requestPath := range []string{
		"/admin/embed.go",
		"/admin/../go.mod",
		"/admin/locales/",
		"/admin/vendor/",
		"/admin/not-found.js",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s unexpectedly returned an embedded source file", requestPath)
		}
		if strings.Contains(recorder.Body.String(), "module kiro-go") {
			t.Fatalf("%s exposed go.mod", requestPath)
		}
	}
}

func TestLegacyAdminRedirectsToEmbeddedUI(t *testing.T) {
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/index-legacy.html", nil))

	if recorder.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusPermanentRedirect)
	}
	if location := recorder.Header().Get("Location"); location != "/admin/" {
		t.Fatalf("Location = %q, want /admin/", location)
	}
}
