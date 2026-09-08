package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const attachmentTestSource = "https://cdn.discordapp.com/attachments/123/456/file.bin?ex=1&hm=signed-secret"

func TestAttachmentUpload(t *testing.T) {
	for _, sourceRoot := range []string{
		"https://cdn.discordapp.com/attachments/123/456/",
		"https://media.discordapp.net:443/ephemeral-attachments/123/456/",
	} {
		t.Run(sourceRoot, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0, 1, 2, 255, '\n', '\r', 128, 42}, 8192)
			filename := "résumé 100%.bin"
			uploadStarted := make(chan struct{})
			cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "ex=1&hm=signed-secret" {
					t.Errorf("unexpected CDN request method, credentials, or query")
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.Header().Set("Content-Type", `application/octet-stream; label="a b"`)
				_, _ = w.Write(payload[:len(payload)/2])
				w.(http.Flusher).Flush()
				select {
				case <-uploadStarted:
					_, _ = w.Write(payload[len(payload)/2:])
				case <-r.Context().Done():
				}
			})
			filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(uploadStarted)
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, payload) {
					t.Errorf("uploaded bytes differ: length %d, read error %v", len(body), err)
				}
				account, password, ok := r.BasicAuth()
				if !ok || account != "sasl-account" || password != "sasl-password" {
					t.Error("upload did not use SASL account credentials")
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/upload" || r.ContentLength != int64(len(payload)) || len(r.TransferEncoding) != 0 {
					t.Error("upload did not use a fixed-length raw POST")
				}
				mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mediaType != "application/octet-stream" || params["label"] != "a b" {
					t.Error("upload content type was not preserved")
				}
				disposition, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
				if err != nil || disposition != "attachment" || params["filename"] != filename {
					t.Errorf("upload filename was not preserved: %q", params["filename"])
				}
				w.Header().Set("Location", "../files/result.bin")
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(filehost.Close)
			uploader := attachmentUploaderForTest(t, cdn, filehost)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := uploader.Upload(ctx, filehost.URL+"/api/upload", sourceRoot+url.PathEscape(filename)+"?ex=1&hm=signed-secret")
			if err != nil || result != filehost.URL+"/files/result.bin" {
				t.Fatalf("Upload = %q, %v", result, err)
			}
		})
	}
}

func TestAttachmentRejectsUnsafeURLsBeforeNetworking(t *testing.T) {
	const endpoint = "https://files.example/upload"
	cases := []struct {
		name     string
		endpoint string
		source   string
	}{
		{"empty endpoint", "", attachmentTestSource},
		{"plain endpoint", "http://files.example/upload", attachmentTestSource},
		{"relative endpoint", "/upload", attachmentTestSource},
		{"missing endpoint host", "https:///upload", attachmentTestSource},
		{"opaque endpoint", "https:files.example/upload", attachmentTestSource},
		{"endpoint credentials", "https://account:secret@files.example/upload", attachmentTestSource},
		{"endpoint fragment", endpoint + "#fragment", attachmentTestSource},
		{"empty endpoint fragment", endpoint + "#", attachmentTestSource},
		{"endpoint space", endpoint + " bad", attachmentTestSource},
		{"endpoint Unicode whitespace", endpoint + "\u00a0bad", attachmentTestSource},
		{"endpoint control", endpoint + "\x00bad", attachmentTestSource},
		{"endpoint malformed escape", endpoint + "%zz", attachmentTestSource},
		{"empty endpoint port", "https://files.example:/upload", attachmentTestSource},
		{"out of range endpoint port", "https://files.example:65536/upload", attachmentTestSource},
		{"empty source", endpoint, ""},
		{"plain source", endpoint, "http://cdn.discordapp.com/attachments/123/456/file.bin"},
		{"source credentials", endpoint, "https://account:secret@cdn.discordapp.com/attachments/123/456/file.bin"},
		{"nonstandard source port", endpoint, "https://cdn.discordapp.com:8443/attachments/123/456/file.bin"},
		{"source host suffix", endpoint, "https://cdn.discordapp.com.evil.example/attachments/123/456/file.bin"},
		{"source subdomain", endpoint, "https://evil.cdn.discordapp.com/attachments/123/456/file.bin"},
		{"source host trailing dot", endpoint, "https://cdn.discordapp.com./attachments/123/456/file.bin"},
		{"source IP", endpoint, "https://127.0.0.1/attachments/123/456/file.bin"},
		{"nonattachment path", endpoint, "https://cdn.discordapp.com/avatars/123/456/file.bin"},
		{"nonnumeric channel", endpoint, "https://cdn.discordapp.com/attachments/abc/456/file.bin"},
		{"nonnumeric attachment", endpoint, "https://cdn.discordapp.com/attachments/123/-456/file.bin"},
		{"missing filename", endpoint, "https://cdn.discordapp.com/attachments/123/456/"},
		{"nested filename", endpoint, "https://cdn.discordapp.com/attachments/123/456/extra/file.bin"},
		{"encoded separator", endpoint, "https://cdn.discordapp.com/attachments/123/456/extra%2Ffile.bin"},
		{"dot filename", endpoint, "https://cdn.discordapp.com/attachments/123/456/."},
		{"parent filename", endpoint, "https://cdn.discordapp.com/attachments/123/456/%2e%2e"},
		{"encoded filename control", endpoint, "https://cdn.discordapp.com/attachments/123/456/file%0abin"},
		{"source fragment", endpoint, attachmentTestSource + "#fragment"},
		{"source whitespace", endpoint, attachmentTestSource + " bad"},
		{"source newline", endpoint, attachmentTestSource + "\nbad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uploader := newAttachmentUploader(RuntimeConfig{})
			var dials atomic.Int32
			for _, client := range []*http.Client{uploader.downloadClient, uploader.uploadClient} {
				transport := client.Transport.(*http.Transport)
				transport.Proxy = nil
				transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("unexpected network request")
				}
				t.Cleanup(client.CloseIdleConnections)
			}
			result, err := uploader.Upload(t.Context(), tc.endpoint, tc.source)
			if err == nil || result != "" || dials.Load() != 0 {
				t.Fatalf("unsafe URL reached network or succeeded: result %q, error %v, dials %d", result, err, dials.Load())
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https:") {
				t.Fatal("error exposed credentials or a URL")
			}
		})
	}
}

func TestAttachmentRejectsRedirects(t *testing.T) {
	for _, leg := range []string{"download", "upload"} {
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
			t.Run(leg+strconv.Itoa(status), func(t *testing.T) {
				var redirected, uploads atomic.Int32
				cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "" {
						t.Error("credentials leaked to Discord")
					}
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
					} else if leg == "download" {
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(status)
						return
					}
					w.Header().Set("Content-Length", "4")
					_, _ = io.WriteString(w, "data")
				})
				filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						if r.Header.Get("Authorization") != "" {
							t.Error("credentials leaked to a redirect target")
						}
						w.Header().Set("Location", "/files/result")
						w.WriteHeader(http.StatusCreated)
						return
					}
					uploads.Add(1)
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
				}))
				t.Cleanup(filehost.Close)
				uploader := attachmentUploaderForTest(t, cdn, filehost)
				result, err := uploader.Upload(t.Context(), filehost.URL+"/upload", attachmentTestSource)
				if err == nil || result != "" || redirected.Load() != 0 || (leg == "download" && uploads.Load() != 0) {
					t.Fatalf("redirect did not fail closed: result %q, error %v, redirects %d, uploads %d", result, err, redirected.Load(), uploads.Load())
				}
			})
		}
	}
}

func TestAttachmentRejectsInvalidResponses(t *testing.T) {
	cases := []struct {
		name           string
		downloadStatus int
		length         string
		contentType    string
		encoding       string
		uploadStatus   int
		location       string
		beforeUpload   bool
	}{
		{name: "download missing", downloadStatus: http.StatusNotFound, beforeUpload: true},
		{name: "partial download", downloadStatus: http.StatusPartialContent, beforeUpload: true},
		{name: "unknown length", length: "chunked", beforeUpload: true},
		{name: "zero length", length: "0", beforeUpload: true},
		{name: "oversize", length: strconv.Itoa(maxAttachmentSize + 1), beforeUpload: true},
		{name: "malformed content type", contentType: "text/plain; broken", beforeUpload: true},
		{name: "non media content type", contentType: "attachment", beforeUpload: true},
		{name: "compressed download", encoding: "gzip", beforeUpload: true},
		{name: "upload not created", uploadStatus: http.StatusOK},
		{name: "upload unauthorized", uploadStatus: http.StatusUnauthorized},
		{name: "upload too large", uploadStatus: http.StatusRequestEntityTooLarge},
		{name: "upload failure", uploadStatus: http.StatusInternalServerError},
		{name: "missing location"},
		{name: "plain location", location: "http://files.example/result"},
		{name: "location credentials", location: "https://account:secret@files.example/result"},
		{name: "location fragment", location: "/result#fragment"},
		{name: "empty location fragment", location: "/result#"},
		{name: "location whitespace", location: "/bad result"},
		{name: "malformed location", location: "%zz"},
		{name: "missing location authority", location: "//"},
		{name: "oversize location", location: "/" + strings.Repeat("x", maxAttachmentURLLength)},
		{name: "short download", length: "100", location: "/result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
				length := tc.length
				if length == "" {
					length = "4"
				}
				contentType := tc.contentType
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				w.Header().Set("Content-Type", contentType)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				if length != "chunked" {
					w.Header().Set("Content-Length", length)
				}
				status := tc.downloadStatus
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				if length == "chunked" {
					w.(http.Flusher).Flush()
				}
				if length != "0" {
					_, _ = io.WriteString(w, "data")
				}
			})
			filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", tc.location)
				status := tc.uploadStatus
				if status == 0 {
					status = http.StatusCreated
				}
				w.WriteHeader(status)
			}))
			t.Cleanup(filehost.Close)
			uploader := attachmentUploaderForTest(t, cdn, filehost)
			result, err := uploader.Upload(t.Context(), filehost.URL+"/upload", attachmentTestSource)
			if err == nil || result != "" {
				t.Fatalf("invalid response succeeded: %q, %v", result, err)
			}
			if tc.beforeUpload && uploads.Load() != 0 {
				t.Fatal("invalid download reached filehost")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https:") {
				t.Fatal("error exposed credentials or a URL")
			}
		})
	}
}

func TestAttachmentEarlyCreatedDoesNotHideShortDownload(t *testing.T) {
	earlyReply := make(chan struct{})
	cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(32<<10))
		_, _ = io.WriteString(w, strings.Repeat("x", 16<<10))
		w.(http.Flusher).Flush()
		select {
		case <-earlyReply:
		case <-r.Context().Done():
		}
	})
	filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Location", "/result")
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		close(earlyReply)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(filehost.Close)
	uploader := attachmentUploaderForTest(t, cdn, filehost)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := uploader.Upload(ctx, filehost.URL+"/upload", attachmentTestSource)
	if err == nil || result != "" {
		t.Fatalf("early 201 hid a truncated attachment: %q, %v", result, err)
	}
	select {
	case <-earlyReply:
	default:
		t.Fatal("upload never reached the early response")
	}
}

func TestAttachmentLocationLengthBoundary(t *testing.T) {
	cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		_, _ = io.WriteString(w, "data")
	})
	filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		size, err := strconv.Atoi(r.URL.Query().Get("size"))
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		location := "/" + strings.Repeat("x", size-len("https://"+r.Host)-1)
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(filehost.Close)
	uploader := attachmentUploaderForTest(t, cdn, filehost)
	for _, size := range []int{maxAttachmentURLLength, maxAttachmentURLLength + 1} {
		location := "/" + strings.Repeat("x", size-len(filehost.URL)-1)
		result, err := uploader.Upload(t.Context(), filehost.URL+"/upload?size="+strconv.Itoa(size), attachmentTestSource)
		if size == maxAttachmentURLLength {
			if err != nil || result != filehost.URL+location {
				t.Fatalf("400-byte URL rejected: %q, %v", result, err)
			}
		} else if err == nil || result != "" {
			t.Fatalf("401-byte URL accepted: %q, %v", result, err)
		}
	}
}

func TestAttachmentTLSBoundaries(t *testing.T) {
	cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		_, _ = io.WriteString(w, "data")
	})
	var uploads atomic.Int32
	filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "/result")
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(filehost.Close)
	t.Run("filehost hostname verification", func(t *testing.T) {
		uploader := attachmentUploaderForTest(t, cdn, filehost)
		transport := uploader.uploadClient.Transport.(*http.Transport)
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, filehost.Listener.Addr().String())
		}
		result, err := uploader.Upload(t.Context(), "https://wrong.example/upload", attachmentTestSource)
		if err == nil || result != "" || uploads.Load() != 0 {
			t.Fatalf("filehost hostname mismatch accepted: %q, %v", result, err)
		}
	})
	t.Run("Ergo roots do not trust CDN", func(t *testing.T) {
		roots := x509.NewCertPool()
		roots.AddCert(cdn.Certificate())
		roots.AddCert(filehost.Certificate())
		uploader := newAttachmentUploader(RuntimeConfig{ErgoRootCAs: roots})
		t.Cleanup(uploader.downloadClient.CloseIdleConnections)
		t.Cleanup(uploader.uploadClient.CloseIdleConnections)
		transport := uploader.downloadClient.Transport.(*http.Transport)
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, cdn.Listener.Addr().String())
		}
		result, err := uploader.Upload(t.Context(), filehost.URL+"/upload", attachmentTestSource)
		if err == nil || result != "" || uploads.Load() != 0 {
			t.Fatalf("Ergo roots trusted Discord: %q, %v", result, err)
		}
	})
}

func TestAttachmentUploadCancellation(t *testing.T) {
	type uploadResult struct {
		url string
		err error
	}
	for _, leg := range []string{"download", "upload"} {
		t.Run(leg, func(t *testing.T) {
			started := make(chan struct{})
			cdn := attachmentCDNServer(t, func(w http.ResponseWriter, r *http.Request) {
				if leg == "download" {
					close(started)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Length", "4")
				_, _ = io.WriteString(w, "data")
			})
			filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				<-r.Context().Done()
			}))
			t.Cleanup(filehost.Close)
			uploader := attachmentUploaderForTest(t, cdn, filehost)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan uploadResult, 1)
			go func() {
				result, err := uploader.Upload(ctx, filehost.URL+"/upload", attachmentTestSource)
				done <- uploadResult{result, err}
			}()
			select {
			case <-started:
			case result := <-done:
				t.Fatalf("request did not stall: %q, %v", result.url, result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case result := <-done:
				if result.err == nil || result.url != "" {
					t.Fatalf("canceled upload succeeded: %q, %v", result.url, result.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled upload did not return promptly")
			}
		})
	}
}

func TestSameHTTPSOrigin(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://files.example/upload", "https://FILES.example:443/voice.ogg", true},
		{"https://files.example:444/upload", "https://files.example/voice.ogg", false},
		{"https://files.example/upload", "https://other.example/voice.ogg", false},
	} {
		if got := sameHTTPSOrigin(parse(tc.a), parse(tc.b)); got != tc.want {
			t.Errorf("sameHTTPSOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func attachmentCDNServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"cdn.discordapp.com", "media.discordapp.net"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func attachmentUploaderForTest(t *testing.T, cdn, filehost *httptest.Server) *attachmentUploader {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(filehost.Certificate())
	uploader := newAttachmentUploader(RuntimeConfig{
		Config: Config{Ergo: ErgoConfig{
			Account:       "sasl-account",
			OperName:      "oper-account",
			TLSServerName: "irc-only.invalid",
		}},
		ErgoPassword: "sasl-password",
		OperPassword: "oper-password",
		ErgoRootCAs:  roots,
	})
	transport := uploader.downloadClient.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(cdn.Certificate())
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, cdn.Listener.Addr().String())
	}
	uploader.uploadClient.Transport.(*http.Transport).Proxy = nil
	t.Cleanup(uploader.downloadClient.CloseIdleConnections)
	t.Cleanup(uploader.uploadClient.CloseIdleConnections)
	return uploader
}
